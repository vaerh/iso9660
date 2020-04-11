package iso9660

type ElToritoPlatform byte
type ElToritoEmul byte

const (
	ElToritoX86 ElToritoPlatform = 0
	ElToritoPPC ElToritoPlatform = 1
	ElToritoMac ElToritoPlatform = 2
	ElToritoEFI ElToritoPlatform = 0xef

	ElToritoNoEmul    ElToritoEmul = 0
	ElToritoFloppy122 ElToritoEmul = 1
	ElToritoFloppy144 ElToritoEmul = 2
	ElToritoFloppy288 ElToritoEmul = 3
	ElToritoHDD       ElToritoEmul = 4
)
