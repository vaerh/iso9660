package iso9660

import (
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
	root map[string]interface{}
}

// NewWriter creates a new ImageWrite.
func NewWriter() (*ImageWriter, error) {
	return &ImageWriter{
		root: make(map[string]interface{}),
	}, nil
}

// Cleanup exists for compatibility. It is not used anymore.
func (iw *ImageWriter) Cleanup() error {
	return nil
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

func manglePath(input string) (string, string) {
	nonEmptySegments := splitPath(path.Clean(input))

	dirSegments := nonEmptySegments[:len(nonEmptySegments)-1]
	name := nonEmptySegments[len(nonEmptySegments)-1]

	for i := 0; i < len(dirSegments); i++ {
		dirSegments[i] = mangleDirectoryName(dirSegments[i])
	}
	name = mangleFileName(name)

	return path.Join(dirSegments...), name
}

func splitPath(input string) []string {
	rawSegments := strings.Split(input, "/")
	var nonEmptySegments []string
	for _, s := range rawSegments {
		if len(s) > 0 {
			nonEmptySegments = append(nonEmptySegments, s)
		}
	}
	return nonEmptySegments
}

// See ECMA-119 7.5
func mangleFileName(input string) string {
	input = strings.ToUpper(input)
	split := strings.Split(input, ".")

	version := "1"
	var filename, extension string
	if len(split) == 1 {
		filename = split[0]
	} else {
		filename = strings.Join(split[:len(split)-1], "_")
		extension = split[len(split)-1]
	}

	// enough characters for the `.ignition` extension
	extension = mangleDString(extension, 8)

	maxRemainingFilenameLength := primaryVolumeFileIdentifierMaxLength - (1 + len(version))
	if len(extension) > 0 {
		maxRemainingFilenameLength -= (1 + len(extension))
	}

	filename = mangleDString(filename, maxRemainingFilenameLength)

	if len(extension) > 0 {
		return filename + "." + extension + ";" + version
	}

	return filename + ";" + version
}

// See ECMA-119 7.6
func mangleDirectoryName(input string) string {
	return mangleDString(input, primaryVolumeDirectoryIdentifierMaxLength)
}

func mangleDString(input string, maxCharacters int) string {
	input = strings.ToUpper(input)

	var mangledString string
	for i := 0; i < len(input) && i < maxCharacters; i++ {
		r := rune(input[i])
		if strings.ContainsRune(dCharacters, r) {
			mangledString += string(r)
		} else {
			mangledString += "_"
		}
	}

	return mangledString
}

func calculateDirChildrenSectors(dir map[string]interface{}) (uint32, error) {
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

	return sectors, nil
}

func fileLengthToSectors(l uint32) uint32 {
	if (l % sectorSize) == 0 {
		return l / sectorSize
	}

	return (l / sectorSize) + 1
}

type writeContext struct {
	root              map[string]interface{}
	wa                io.WriterAt
	timestamp         RecordingTimestamp
	freeSectorPointer uint32
	itemsToWrite      *list.List     // simple fifo used during
	items             []*itemToWrite // items in the right order for final write
}

// allocSectors will allocate a number of sectors and return the first free position
func (wc *writeContext) allocSectors(count uint32) uint32 {
	res := wc.freeSectorPointer
	// no need to use atomic here
	wc.freeSectorPointer += count
	return res
}

func (wc *writeContext) createDEForRoot() (*DirectoryEntry, error) {
	extentLengthInSectors, err := calculateDirChildrenSectors(wc.root)
	if err != nil {
		return nil, err
	}

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
	var writeOffset uint32

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

	n, err := wc.wa.WriteAt(currentDEData, int64((targetSector*sectorSize)+writeOffset))
	if err != nil {
		return err
	}
	writeOffset += uint32(n)
	n, err = wc.wa.WriteAt(parentDEData, int64((targetSector*sectorSize)+writeOffset))
	if err != nil {
		return err
	}
	writeOffset += uint32(n)

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
			extentLengthInSectors, err = calculateDirChildrenSectors(cV)
			if err != nil {
				return err
			}
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

		// queue this child for processing
		wc.itemsToWrite.PushBack(itemToWrite{
			value:        c,
			dirPath:      path.Join(dirPath, name),
			ownEntry:     de,
			parentEntry:  ownEntry,
			targetSector: uint32(de.ExtentLocation),
		})

		data, err := de.MarshalBinary()
		if err != nil {
			return err
		}

		if writeOffset+uint32(len(data)) > sectorSize {
			// unless we reached the exact end of the sector
			if writeOffset < sectorSize {
				// write the 0 size marker, telling the reader that the next entry is on the next sector
				n, err = wc.wa.WriteAt([]byte{}, int64((targetSector*sectorSize)+writeOffset))
				if err != nil {
					return err
				}
			}

			// skip to the next sector
			writeOffset = 0
			targetSector += 1
		}

		n, err = wc.wa.WriteAt(data, int64((targetSector*sectorSize)+writeOffset))
		if err != nil {
			return err
		}
		writeOffset += uint32(n)
	}

	// unless we reached the exact end of the sector
	if writeOffset < sectorSize {
		// write the 0 size marker, telling the reader that the next entry is on the next sector
		n, err = wc.wa.WriteAt([]byte{}, int64((targetSector*sectorSize)+writeOffset))
		if err != nil {
			return err
		}
	}

	return nil
}

func (wc *writeContext) processFile(dirPath string, buf genericBuffer, targetSector uint32) error {
	if buf.Size() > int64(math.MaxUint32) {
		return ErrFileTooLarge
	}

	buffer := make([]byte, sectorSize)

	for bytesLeft := uint32(buf.Size()); bytesLeft > 0; {
		var toRead uint32
		if bytesLeft < sectorSize {
			toRead = bytesLeft
		} else {
			toRead = sectorSize
		}

		if _, err := io.ReadAtLeast(buf, buffer, int(toRead)); err != nil {
			return err
		}

		if _, err := wc.wa.WriteAt(buffer, int64(targetSector*sectorSize)); err != nil {
			return err
		}

		targetSector++
		bytesLeft -= toRead
	}

	buf.Close()

	return nil
}

func (wc *writeContext) writeAll() error {
	for item := wc.itemsToWrite.Front(); wc.itemsToWrite.Len() > 0; item = wc.itemsToWrite.Front() {
		it := item.Value.(itemToWrite)
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

func (iw *ImageWriter) WriteTo(wa io.WriterAt, volumeIdentifier string) error {
	buffer := make([]byte, sectorSize)
	var err error

	// write 16 sectors of zeroes
	for i := uint32(0); i < 16; i++ {
		if _, err = wa.WriteAt(buffer, int64(sectorSize*i)); err != nil {
			return err
		}
	}

	now := time.Now()

	wc := writeContext{
		root:              iw.root,
		wa:                wa,
		timestamp:         RecordingTimestamp{},
		freeSectorPointer: 18, // system area (16) + 2 volume descriptors
		itemsToWrite:      list.New(),
	}

	rootDE, err := wc.createDEForRoot()
	if err != nil {
		return fmt.Errorf("creating root directory descriptor: %s", err)
	}

	wc.itemsToWrite.PushBack(itemToWrite{
		value:        wc.root,
		dirPath:      "",
		ownEntry:     rootDE,
		parentEntry:  rootDE,
		targetSector: uint32(rootDE.ExtentLocation),
	})

	if err = wc.writeAll(); err != nil {
		return fmt.Errorf("writing files: %s", err)
	}

	pvd := volumeDescriptor{
		Header: volumeDescriptorHeader{
			Type:       volumeTypePrimary,
			Identifier: standardIdentifierBytes,
			Version:    1,
		},
		Primary: &PrimaryVolumeDescriptorBody{
			SystemIdentifier:              runtime.GOOS,
			VolumeIdentifier:              volumeIdentifier,
			VolumeSpaceSize:               int32(wc.freeSectorPointer),
			VolumeSetSize:                 1,
			VolumeSequenceNumber:          1,
			LogicalBlockSize:              int16(sectorSize),
			PathTableSize:                 0,
			TypeLPathTableLoc:             0,
			OptTypeLPathTableLoc:          0,
			TypeMPathTableLoc:             0,
			OptTypeMPathTableLoc:          0,
			RootDirectoryEntry:            rootDE,
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
		},
	}
	if buffer, err = pvd.MarshalBinary(); err != nil {
		return err
	}
	if _, err = wa.WriteAt(buffer, int64(sectorSize*16)); err != nil {
		return err
	}

	terminator := volumeDescriptor{
		Header: volumeDescriptorHeader{
			Type:       volumeTypeTerminator,
			Identifier: standardIdentifierBytes,
			Version:    1,
		},
	}
	if buffer, err = terminator.MarshalBinary(); err != nil {
		return err
	}
	if _, err = wa.WriteAt(buffer, int64(sectorSize*17)); err != nil {
		return err
	}

	return nil
}
