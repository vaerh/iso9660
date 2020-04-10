package iso9660

type itemMeta struct {
	dirPath      string
	ownEntry     *DirectoryEntry
	parentEntry  *DirectoryEntry
	targetSector uint32
}

func (i *itemMeta) set(dirPath string, own, parent *DirectoryEntry, targetSector uint32) {
	i.dirPath = dirPath
	i.ownEntry = own
	i.parentEntry = parent
	i.targetSector = targetSector
}
