package cmd

// fileStamp identifies one version of a file: which file (device and inode),
// its content and metadata change times, and its size. mtime alone is not
// enough: a rewrite can restore mtime, and chmod changes neither mtime nor
// size. ctime cannot be set by userspace, and a rename-replace changes the
// inode.
type fileStamp struct {
	dev, ino     uint64
	mtime, ctime int64
	size         int64
}
