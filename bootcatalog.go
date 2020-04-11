package iso9660

import (
	"bytes"
	"encoding/binary"
)

// ElTorito boot catalog
// see: https://dev.lovelyhq.com/libburnia/libisofs/raw/master/doc/boot_sectors.txt

type BootCatalogEntry struct {
	Platform      ElToritoPlatform
	BootMedia     ElToritoEmul // 0=NoEmul, 2=1.44MB disk, 4=HDD
	BootInfoTable bool
	file          Item
}

// encodeBootCatalogs must be called after prepareAll so that targetSector is
// populated.
func encodeBootCatalogs(e []*BootCatalogEntry) ([]byte, error) {
	// transform a list of catalog entries into binary catalog
	buf := &bytes.Buffer{}

	cnt := len(e)

	for i, b := range e {
		if i == 0 {
			// Validation Entry
			buf.Write([]byte{1, byte(b.Platform), 0, 0}) // 4 bytes
			// manuf_dev
			buf.Write(make([]byte, 24)) // 24 bytes
			// checksum
			buf.Write([]byte{0x00, 0x00}) // 2 bytes
			// 0x55 0xaa (bootable)
			buf.Write([]byte{0x55, 0xaa}) // 2 bytes

			// compute checksum
			v := doBootCatalogChecksum(buf.Bytes())
			copy(buf.Bytes()[28:30], v)
		} else {
			// Section Header Entry
			headInd := byte(0x90)
			if i == cnt-1 {
				headInd = 0x91
			}
			buf.Write([]byte{headInd, byte(b.Platform), 1, 0})
			buf.Write(make([]byte, 28))
		}

		// Initial/Default Entry or Section Entry
		buf.Write([]byte{0x88, byte(b.BootMedia), 0x00, 0x00}) // 4 bytes
		buf.Write([]byte{0, 0})                                // 2 bytes: sys_type, unused

		// sec count depends if we are a uefi file or not (uefi needs file size)
		if b.Platform == 0xef {
			// UEFI
			siz := b.file.Size()
			sizSec := uint16(siz / 512)
			if siz%512 != 0 {
				sizSec += 1
			}

			binary.Write(buf, binary.LittleEndian, sizSec) // 2 bytes
		} else {
			binary.Write(buf, binary.LittleEndian, uint16(4)) // 2 bytes
		}

		// load_rba
		binary.Write(buf, binary.LittleEndian, uint32(b.file.meta().targetSector)) // 4 bytes

		buf.Write(make([]byte, 20)) // "Vendor unique selection criteria."

		if b.BootInfoTable {
			b.performInfoTable()
		}
	}
	return buf.Bytes(), nil
}

func doBootCatalogChecksum(b []byte) []byte {
	var v uint16

	for i := 0; i < len(b); i += 2 {
		v -= binary.LittleEndian.Uint16(b[i : i+2])
	}

	res := make([]byte, 2)
	binary.LittleEndian.PutUint16(res, v)
	return res
}

func doElToritoTableChecksum(in []byte) (r uint32) {
	var i int
	for i < len(in)-4 {
		r += binary.LittleEndian.Uint32(in[i : i+4])
		i += 4
	}
	if i != len(in) {
		// file length not multiple of 4
		buf := make([]byte, 4)
		copy(buf, in[i:])
		r += binary.LittleEndian.Uint32(buf)
	}
	return
}

func (b *BootCatalogEntry) performInfoTable() {
	// alter file in b.file (a *bufferHndlr) to include boot info table insertion
	// see: man mkisofs under EL TORITO BOOT INFORMATION TABLE
	f := b.file.(*bufferHndlr)
	binary.LittleEndian.PutUint32(f.d[8:12], 16)                                 // LBA of primary volume descriptor (always 16)
	binary.LittleEndian.PutUint32(f.d[12:16], f.meta().targetSector)             // LBA of boot file
	binary.LittleEndian.PutUint32(f.d[16:20], uint32(f.Size()))                  // Boot file length in bytes
	binary.LittleEndian.PutUint32(f.d[20:24], doElToritoTableChecksum(f.d[64:])) // 32bit checksum
}
