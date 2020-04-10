package iso9660

import "bytes"

type itemDir struct {
	children map[string]Item
	buf      *bytes.Buffer
	m        itemMeta
}

func newDir() *itemDir {
	res := &itemDir{
		children: make(map[string]Item),
		buf:      &bytes.Buffer{},
	}
	return res
}

func (d *itemDir) Read(p []byte) (int, error) {
	return d.buf.Read(p)
}

func (d *itemDir) sectors() uint32 {
	var sectors uint32
	var currentSectorOccupied uint32 = 68 // the 0x00 and 0x01 entries

	for name := range d.children {
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

func (d *itemDir) Size() int64 {
	return int64(d.sectors() * sectorSize)
}

func (d *itemDir) Close() error {
	return nil
}

func (d *itemDir) meta() *itemMeta {
	return &d.m
}
