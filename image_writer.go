package iso9660

import (
	"bytes"
	"container/list"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	primaryVolumeDirectoryIdentifierMaxLength = 31 // ECMA-119 7.6.3
	primaryVolumeFileIdentifierMaxLength      = 30 // ECMA-119 7.5
)

var (
	// ErrFileTooLarge is returned when trying to process a file of size greater
	// than 4GB, which due to the 32-bit address limitation is not possible
	// except with ISO 9660-Level 3
	ErrFileTooLarge = errors.New("file is exceeding the maximum file size of 4GB")
	ErrIsDir        = errors.New("is a directory")
)

// ImageWriter is responsible for staging an image's contents
// and writing them to an image.
type ImageWriter struct {
	Primary *PrimaryVolumeDescriptorBody
	Catalog string // Catalog is the path of the boot catalog on disk. Defaults to "BOOT.CAT"

	root        *itemDir
	vd          []*volumeDescriptor
	boot        []*bootCatalogEntry // boot entries
	lookupTable map[string]Item     // allows quick lookup of any given item
}

// NewWriter creates a new ImageWrite.
func NewWriter() (*ImageWriter, error) {
	now := time.Now()
	Primary := &PrimaryVolumeDescriptorBody{
		SystemIdentifier:              runtime.GOOS,
		VolumeIdentifier:              "UNNAMED",
		VolumeSpaceSize:               0, // this will be calculated upon finalization of disk
		VolumeSetSize:                 1,
		VolumeSequenceNumber:          1,
		LogicalBlockSize:              int16(sectorSize),
		PathTableSize:                 0,
		TypeLPathTableLoc:             0,
		OptTypeLPathTableLoc:          0,
		TypeMPathTableLoc:             0,
		OptTypeMPathTableLoc:          0,
		RootDirectoryEntry:            nil, // this will be calculated upon finalization of disk
		VolumeSetIdentifier:           "",
		PublisherIdentifier:           "",
		DataPreparerIdentifier:        "",
		ApplicationIdentifier:         "github.com/KarpelesLab/iso9660",
		CopyrightFileIdentifier:       "",
		AbstractFileIdentifier:        "",
		BibliographicFileIdentifier:   "",
		VolumeCreationDateAndTime:     VolumeDescriptorTimestampFromTime(now),
		VolumeModificationDateAndTime: VolumeDescriptorTimestampFromTime(now),
		VolumeExpirationDateAndTime:   VolumeDescriptorTimestamp{},
		VolumeEffectiveDateAndTime:    VolumeDescriptorTimestampFromTime(now),
		FileStructureVersion:          1,
		ApplicationUsed:               [512]byte{},
	}

	return &ImageWriter{
		root:        newDir(),
		Primary:     Primary,
		Catalog:     "BOOT.CAT",
		lookupTable: make(map[string]Item),
		vd: []*volumeDescriptor{
			{
				Header: volumeDescriptorHeader{
					Type:       volumeTypePrimary,
					Identifier: standardIdentifierBytes,
					Version:    1,
				},
				Primary: Primary,
			},
		},
	}, nil
}

func (iw *ImageWriter) AddBootEntry(platformId byte, bootMedia byte, filePath string, data Item) error {
	directoryPath, fileName := manglePath(filePath)

	pos, err := iw.getDir(directoryPath)
	if err != nil {
		return err
	}

	if _, ok := pos.children[fileName]; ok {
		// duplicate
		return os.ErrExist
	}

	item, err := NewItemReader(data)
	if err != nil {
		return err
	}

	dirPath := path.Join(directoryPath, fileName)
	item.meta().dirPath = dirPath
	iw.lookupTable[dirPath] = item
	pos.children[fileName] = item

	// add boot record
	iw.boot = append(iw.boot, &bootCatalogEntry{
		platformId: platformId,
		bootMedia:  bootMedia,
		file:       item,
	})
	return nil
}

func (iw *ImageWriter) getDir(directoryPath string) (*itemDir, error) {
	dp := strings.Split(directoryPath, "/")
	pos := iw.root
	for _, seg := range dp {
		if v, ok := pos.children[seg]; ok {
			if rV, ok := v.(*itemDir); ok {
				pos = rV
				continue
			}
			// trying to create a directory on top of a file → problem
			return nil, ErrIsDir
		}
		// not found → add
		n := newDir()
		pos.children[seg] = n
		pos = n
	}

	return pos, nil
}

// AddFile adds a file to the ImageWriter.
// All path components are mangled to match basic ISO9660 filename requirements.
func (iw *ImageWriter) AddFile(data io.Reader, filePath string) error {
	directoryPath, fileName := manglePath(filePath)

	pos, err := iw.getDir(directoryPath)
	if err != nil {
		return err
	}

	if _, ok := pos.children[fileName]; ok {
		// duplicate
		return os.ErrExist
	}

	item, err := NewItemReader(data)
	if err != nil {
		return err
	}

	dirPath := path.Join(directoryPath, fileName)
	item.meta().dirPath = dirPath
	iw.lookupTable[dirPath] = item
	pos.children[fileName] = item
	return nil
}

// AddLocalFile adds a file to the ImageWriter from the local filesystem.
// localPath must be an existing and readable file, and filePath will be the path
// on the ISO image.
func (iw *ImageWriter) AddLocalFile(localPath, filePath string) error {
	buf, err := NewItemFile(localPath)
	if err != nil {
		return fmt.Errorf("unable to add local file: %w", err)
	}

	return iw.AddFile(buf, filePath)
}

func recursiveDirSectorCount(dir *itemDir) uint32 {
	// count sectors required for everything in a given dir (typically root)
	sec := dir.sectors() // own data space

	for _, sub := range dir.children {
		switch v := sub.(type) {
		case *itemDir:
			sec += recursiveDirSectorCount(v)
		default:
			sec += v.sectors()
		}
	}

	return sec
}

type writeContext struct {
	iw                *ImageWriter
	w                 io.Writer
	timestamp         RecordingTimestamp
	freeSectorPointer uint32
	itemsToWrite      *list.List // simple fifo used during
	items             []Item     // items in the right order for final write
	writeSecPos       uint32
	emptySector       []byte // a sector-sized buffer of zeroes
}

// allocSectors will allocate a number of sectors and return the first free position
func (wc *writeContext) allocSectors(count uint32) uint32 {
	res := wc.freeSectorPointer
	// no need to use atomic here
	wc.freeSectorPointer += count
	return res
}

func (wc *writeContext) createDEForRoot() (*DirectoryEntry, error) {
	extentLengthInSectors := wc.iw.root.sectors()

	extentLocation := wc.allocSectors(extentLengthInSectors)
	de := &DirectoryEntry{
		ExtendedAtributeRecordLength: 0,
		ExtentLocation:               int32(extentLocation),
		ExtentLength:                 int32(extentLengthInSectors * sectorSize),
		RecordingDateTime:            wc.timestamp,
		FileFlags:                    dirFlagDir,
		FileUnitSize:                 0, // 0 for non-interleaved write
		InterleaveGap:                0, // not interleaved
		VolumeSequenceNumber:         1, // we only have one volume
		Identifier:                   string([]byte{0}),
		SystemUse:                    []byte{},
	}
	return de, nil
}

func (wc *writeContext) processDirectory(dir *itemDir, ownEntry *DirectoryEntry, parentEntry *DirectoryEntry, targetSector uint32) error {
	buf := dir.buf
	bufPos := 0

	currentDE := ownEntry.Clone()
	currentDE.Identifier = string([]byte{0})
	parentDE := parentEntry.Clone()
	parentDE.Identifier = string([]byte{1})

	currentDEData, err := currentDE.MarshalBinary()
	if err != nil {
		return err
	}
	parentDEData, err := parentDE.MarshalBinary()
	if err != nil {
		return err
	}

	n, err := buf.Write(currentDEData)
	if err != nil {
		return err
	}
	bufPos += n

	n, err = buf.Write(parentDEData)
	if err != nil {
		return err
	}
	bufPos += n

	// here we need to proceed in alphabetical order so tests aren't broken
	names := make([]string, 0, len(dir.children))
	for name := range dir.children {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	for _, name := range names {
		c := dir.children[name]

		var (
			fileFlags             byte
			extentLengthInSectors uint32
			extentLength          uint32
		)

		if cV, ok := c.(*itemDir); ok {
			extentLengthInSectors = cV.sectors()
			fileFlags = dirFlagDir
			extentLength = extentLengthInSectors * sectorSize
		} else if cV, ok := c.(Item); ok {
			if cV.Size() > int64(math.MaxUint32) {
				return ErrFileTooLarge
			}
			extentLength = uint32(cV.Size())
			extentLengthInSectors = cV.sectors()

			fileFlags = 0
		} else {
			panic("this should not happen")
		}

		extentLocation := wc.allocSectors(extentLengthInSectors)
		de := &DirectoryEntry{
			ExtendedAtributeRecordLength: 0,
			ExtentLocation:               int32(extentLocation),
			ExtentLength:                 int32(extentLength),
			RecordingDateTime:            wc.timestamp,
			FileFlags:                    fileFlags,
			FileUnitSize:                 0, // 0 for non-interleaved write
			InterleaveGap:                0, // not interleaved
			VolumeSequenceNumber:         1, // we only have one volume
			Identifier:                   name,
			SystemUse:                    []byte{},
		}

		c.meta().set(de, ownEntry, uint32(de.ExtentLocation))

		// queue this child for processing
		wc.itemsToWrite.PushBack(c)

		data, err := de.MarshalBinary()
		if err != nil {
			return err
		}

		if uint32(bufPos+len(data)) > sectorSize {
			// unless we reached the exact end of the sector
			if uint32(bufPos) < sectorSize {
				// need to add some bytes
				buf.Write(wc.emptySector[:sectorSize-uint32(bufPos)])
			}
			bufPos = 0
		}

		_, err = buf.Write(data)
		if err != nil {
			return err
		}
	}

	// unless we reached the exact end of the sector
	wc.items = append(wc.items, dir)

	return nil
}

func (wc *writeContext) processFile(buf Item, targetSector uint32) error {
	if buf.Size() > int64(math.MaxUint32) {
		return ErrFileTooLarge
	}

	wc.items = append(wc.items, buf)

	return nil
}

func (wc *writeContext) processAll() error {
	// Generate disk header
	rootDE, err := wc.createDEForRoot()
	if err != nil {
		return fmt.Errorf("creating root directory descriptor: %s", err)
	}

	// store rootDE pointer in primary
	wc.iw.Primary.RootDirectoryEntry = rootDE
	wc.iw.root.meta().ownEntry = rootDE
	wc.iw.root.meta().parentEntry = rootDE
	wc.iw.root.meta().targetSector = uint32(rootDE.ExtentLocation)

	// Write disk data
	wc.itemsToWrite.PushBack(wc.iw.root)

	for item := wc.itemsToWrite.Front(); wc.itemsToWrite.Len() > 0; item = wc.itemsToWrite.Front() {
		it := item.Value.(Item)
		var err error
		if cV, ok := it.(*itemDir); ok {
			err = wc.processDirectory(cV, it.meta().ownEntry, it.meta().parentEntry, it.meta().targetSector)
		} else {
			err = wc.processFile(it, it.meta().targetSector)
		}

		if err != nil {
			return fmt.Errorf("processing %s: %s", it.meta().dirPath, err)
		}

		wc.itemsToWrite.Remove(item)
	}

	return nil
}

// writeSector writes one or more sector(s) to the stream, checking the passed
// position is correct. If buffer is not rounded to a sector position, extra
// zeroes will be written to disk.
func (wc *writeContext) writeSector(buffer []byte, sector uint32) error {
	// ensure our position in the stream is correct
	if sector != wc.writeSecPos {
		// invalid location
		return errors.New("invalid write: sector position is not valid")
	}
	_, err := wc.w.Write(buffer)
	if err != nil {
		return err
	}

	secCnt := uint32(len(buffer)) / sectorSize
	if secBytes := uint32(len(buffer)) % sectorSize; secBytes != 0 {
		secCnt += 1
		// add zeroes using wc.emptySector (which is a sector-sized buffer of zeroes)
		extra := sectorSize - secBytes
		wc.w.Write(wc.emptySector[:extra])
	}

	wc.writeSecPos += secCnt
	return nil
}

// writeSectorBuf will copy the given buffer to the image
func (wc *writeContext) writeSectorBuf(buf Item) error {
	n, err := io.Copy(wc.w, buf)
	if err != nil {
		return err
	}

	secCnt := uint32(n) / sectorSize
	if secBytes := uint32(n) % sectorSize; secBytes != 0 {
		secCnt += 1
		// add zeroes using wc.emptySector (which is a sector-sized buffer of zeroes)
		extra := sectorSize - secBytes
		wc.w.Write(wc.emptySector[:extra])
	}

	wc.writeSecPos += secCnt
	return nil
}

func (wc *writeContext) writeDescriptor(pvd *volumeDescriptor, sector uint32) error {
	if buffer, err := pvd.MarshalBinary(); err != nil {
		return err
	} else {
		return wc.writeSector(buffer, sector)
	}
}

func (iw *ImageWriter) WriteTo(w io.Writer) error {
	vd := iw.vd
	var (
		err error
		// variables used for boot
		boot    *BootVolumeDescriptorBody
		bootCat []byte
	)

	if len(iw.boot) > 0 {
		// we need a boot catalog, store info
		boot = &BootVolumeDescriptorBody{}
		bootCat = make([]byte, 2048)

		// add boot catalog
		err = iw.AddFile(&bufHndlr{Reader: bytes.NewReader(bootCat)}, iw.Catalog)
		if err != nil {
			return err
		}

		vd = append(vd, &volumeDescriptor{
			Header: volumeDescriptorHeader{
				Type:       volumeTypeBoot,
				Identifier: standardIdentifierBytes,
				Version:    1,
			},
			Boot: boot,
		})
	}

	// generate vd list with terminator
	vd = append(vd, &volumeDescriptor{
		Header: volumeDescriptorHeader{
			Type:       volumeTypeTerminator,
			Identifier: standardIdentifierBytes,
			Version:    1,
		},
	})

	wc := writeContext{
		iw:                iw,
		w:                 w,
		timestamp:         RecordingTimestamp{},
		freeSectorPointer: uint32(16 + len(vd)), // system area (16) + descriptors
		itemsToWrite:      list.New(),
		writeSecPos:       0,
		emptySector:       make([]byte, sectorSize),
	}

	// configure volume space size
	iw.Primary.VolumeSpaceSize = int32(16 + uint32(len(vd)) + recursiveDirSectorCount(iw.root))

	// processAll() will prepare the data to be written
	if err = wc.processAll(); err != nil {
		return fmt.Errorf("writing files: %s", err)
	}

	if len(iw.boot) > 0 {
		// we have a boot catalog to make!
		// First, grab the location of boot catalog
		bootCatInfo := wc.iw.lookupTable[iw.Catalog]
		binary.LittleEndian.PutUint32(boot.BootSystemUse[:4], bootCatInfo.meta().targetSector)

		// generate catalog
		data, err := encodeBootCatalogs(iw.boot)
		if err != nil {
			return err
		}

		// overwrite bootCat with data so it will be written to disk
		copy(bootCat, data)
	}

	// write 16 sectors of zeroes
	for i := uint32(0); i < 16; i++ {
		if err = wc.writeSector(wc.emptySector, i); err != nil {
			return err
		}
	}

	sector := uint32(16)
	for _, pvd := range vd {
		if err = wc.writeDescriptor(pvd, sector); err != nil {
			return err
		}
		sector += 1
	}

	// this actually writes the data to the disk
	for _, buf := range wc.items {
		err = wc.writeSectorBuf(buf)
		if err != nil {
			return err
		}
		buf.Close()
	}

	return nil
}
