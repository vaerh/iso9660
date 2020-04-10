package iso9660

// ElTorito boot catalog
// see: https://dev.lovelyhq.com/libburnia/libisofs/raw/master/doc/boot_sectors.txt

type bootCatalogEntry struct {
	platformId byte   // 0x00=PC 0xFE=UEFI
	bootMedia  byte   // 0=NoEmul, 2=1.44MB disk, 4=HDD
	file       string // file path on CD
}
