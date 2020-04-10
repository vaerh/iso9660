package iso9660

import (
	"bytes"
	"container/list"
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
	root    map[string]interface{}
	Primary *PrimaryVolumeDescriptorBody
	vd      []*volumeDescriptor
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
		root:    make(map[string]interface{}),
		Primary: Primary,
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

// Cleanup exists for compatibility. It is not used anymore.
func (iw *ImageWriter) Cleanup() error {
	return nil
}

// AddBoot will add a boot volume descriptor to the image
func (iw *ImageWriter) AddBoot(boot *BootVolumeDescriptorBody) {
	iw.vd = append(iw.vd, &volumeDescriptor{
		Header: volumeDescriptorHeader{
			Type:       volumeTypeBoot,
			Identifier: standardIdentifierBytes,
			Version:    1,
		},
		Boot: boot,
	})
}

func (iw *ImageWriter) getDir(directoryPath string) (map[string]interface{}, error) {
	dp := strings.Split(directoryPath, "/")
	pos := iw.root
	for _, seg := range dp {
		if v, ok := pos[seg]; ok {
			if rV, ok := v.(map[string]interface{}); ok {
				pos = rV
				continue
			}
			// trying to create a directory on top of a file → problem
			return nil, ErrIsDir
		}
		// not found → add
		n := make(map[string]interface{})
		pos[seg] = n
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

	if _, ok := pos[fileName]; ok {
		// duplicate
		return os.ErrExist
	}

	pos[fileName] = newBuffer(data)
	return nil
}

// AddLocalFile adds a file to the ImageWriter from the local filesystem.
// localPath must be an existing and readable file, and filePath will be the path
// on the ISO image.
func (iw *ImageWriter) AddLocalFile(localPath, filePath string) error {
	buf, err := newFileBuffer(localPath)
	if err != nil {
		return fmt.Errorf("unable to add local file: %w", err)
	}

	directoryPath, fileName := manglePath(filePath)

	pos, err := iw.getDir(directoryPath)
	if err != nil {
		return err
	}

	if _, ok := pos[fileName]; ok {
		// duplicate
		return os.ErrExist
	}

	pos[fileName] = buf
	return nil
}

// calculateDirChildrenSectors returns size of a given dir entry
func calculateDirChildrenSectors(dir map[string]interface{}) uint32 {
	var sectors uint32
	var currentSectorOccupied uint32 = 68 // the 0x00 and 0x01 entries

	for name := range dir {
		identifierLen := len(name)
		idPaddingLen := (identifierLen + 1) % 2
		entryLength := uint32(33 + identifierLen + idPaddingLen)

		if currentSectorOccupied+entryLength > sectorSize {
			sectors += 1
			currentSectorOccupied = entryLength
		} else {
			currentSectorOccupied += entryLength
		}
	}

	if currentSectorOccupied > 0 {
		sectors += 1
	}

	return sectors
}

// fileLengthToSectors returns size of a file in sectors
func fileLengthToSectors(l uint32) uint32 {
	if (l % sectorSize) == 0 {
		return l / sectorSize
	}

	return (l / sectorSize) + 1
}

func recursiveDirSectorCount(dir map[string]interface{}) uint32 {
	// count sectors required for everything in a given dir (typically root)
	sec := calculateDirChildrenSectors(dir) // own data space

	for _, sub := range dir {
		switch v := sub.(type) {
		case map[string]interface{}:
			sec += recursiveDirSectorCount(v)
		case genericBuffer:
			sec += fileLengthToSectors(uint32(v.Size()))
		default:
			panic("this should not happen")
		}
	}

	return sec
}

type writeContext struct {
	iw                *ImageWriter
	w                 io.Writer
	timestamp         RecordingTimestamp
	freeSectorPointer uint32
	itemsToWrite      *list.List              // simple fifo used during
	items             []genericBuffer         // items in the right order for final write
	lookupTable       map[string]*itemToWrite // allows quick lookup of any given item
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
	extentLengthInSectors := calculateDirChildrenSectors(wc.iw.root)

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

type itemToWrite struct {
	value        interface{} // can be map[string]interface{} or genericBuffer
	dirPath      string
	ownEntry     *DirectoryEntry
	parentEntry  *DirectoryEntry
	targetSector uint32
}

func (wc *writeContext) processDirectory(dirPath string, dir map[string]interface{}, ownEntry *DirectoryEntry, parentEntry *DirectoryEntry, targetSector uint32) error {
	buf := &bytes.Buffer{}

	currentDE := ownEntry.Clone()
	currentDE.Identifier = string([]byte{0})
	parentDE := ownEntry.Clone()
	parentDE.Identifier = string([]byte{1})

	currentDEData, err := currentDE.MarshalBinary()
	if err != nil {
		return err
	}
	parentDEData, err := parentDE.MarshalBinary()
	if err != nil {
		return err
	}

	_, err = buf.Write(currentDEData)
	if err != nil {
		return err
	}
	_, err = buf.Write(parentDEData)
	if err != nil {
		return err
	}

	// here we need to proceed in alphabetical order so tests aren't broken
	names := make([]string, 0, len(dir))
	for name := range dir {
		names = append(names, name)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })

	for _, name := range names {
		c := dir[name]

		var (
			fileFlags             byte
			extentLengthInSectors uint32
			extentLength          uint32
		)

		if cV, ok := c.(map[string]interface{}); ok {
			extentLengthInSectors = calculateDirChildrenSectors(cV)
			fileFlags = dirFlagDir
			extentLength = extentLengthInSectors * sectorSize
		} else if cV, ok := c.(genericBuffer); ok {
			if cV.Size() > int64(math.MaxUint32) {
				return ErrFileTooLarge
			}
			extentLength = uint32(cV.Size())
			extentLengthInSectors = fileLengthToSectors(extentLength)

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

		dirPath := path.Join(dirPath, name)

		// queue this child for processing
		item := &itemToWrite{
			value:        c,
			dirPath:      dirPath,
			ownEntry:     de,
			parentEntry:  ownEntry,
			targetSector: uint32(de.ExtentLocation),
		}

		wc.itemsToWrite.PushBack(item)
		wc.lookupTable[dirPath] = item

		data, err := de.MarshalBinary()
		if err != nil {
			return err
		}

		if uint32(buf.Len()+len(data)) > sectorSize {
			// unless we reached the exact end of the sector
			wc.items = append(wc.items, newBuffer(buf))
			buf = &bytes.Buffer{} // do not use buf.Reset() to ensure we have a new memory area
		}

		_, err = buf.Write(data)
		if err != nil {
			return err
		}
	}

	// unless we reached the exact end of the sector
	if buf.Len() > 0 {
		wc.items = append(wc.items, newBuffer(buf))
	}

	return nil
}

func (wc *writeContext) processFile(dirPath string, buf genericBuffer, targetSector uint32) error {
	if buf.Size() > int64(math.MaxUint32) {
		return ErrFileTooLarge
	}

	wc.items = append(wc.items, buf)

	return nil
}

func (wc *writeContext) processAll() error {
	for item := wc.itemsToWrite.Front(); wc.itemsToWrite.Len() > 0; item = wc.itemsToWrite.Front() {
		it := item.Value.(*itemToWrite)
		var err error
		if cV, ok := it.value.(map[string]interface{}); ok {
			err = wc.processDirectory(it.dirPath, cV, it.ownEntry, it.parentEntry, it.targetSector)
		} else if cV, ok := it.value.(genericBuffer); ok {
			err = wc.processFile(it.dirPath, cV, it.targetSector)
		} else {
			panic("shouldn't happen")
		}

		if err != nil {
			return fmt.Errorf("processing %s: %s", it.dirPath, err)
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
func (wc *writeContext) writeSectorBuf(buf genericBuffer) error {
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
	var err error

	vd := iw.vd

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
		lookupTable:       make(map[string]*itemToWrite),
	}

	// Generate disk header
	rootDE, err := wc.createDEForRoot()
	if err != nil {
		return fmt.Errorf("creating root directory descriptor: %s", err)
	}

	// configure volume space size
	iw.Primary.VolumeSpaceSize = int32(16 + uint32(len(vd)) + recursiveDirSectorCount(iw.root))
	// store rootDE pointer in primary
	iw.Primary.RootDirectoryEntry = rootDE

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

	// Write disk data
	wc.itemsToWrite.PushBack(&itemToWrite{
		value:        iw.root,
		dirPath:      "",
		ownEntry:     rootDE,
		parentEntry:  rootDE,
		targetSector: uint32(rootDE.ExtentLocation),
	})

	// processAll() will actually prepare the data to be written
	if err = wc.processAll(); err != nil {
		return fmt.Errorf("writing files: %s", err)
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
