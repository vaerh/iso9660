package iso9660

import (
	"bytes"
	"io"
	"os"
)

type genericBuffer interface {
	io.Reader
	Size() int64
}

// fileHandler returns a generic interface for various kind of files or buffers
func newBuffer(in io.Reader) genericBuffer {
	switch v := in.(type) {
	case genericBuffer:
		return v
	case *os.File:
		return &fileHndlr{v}
	case *bytes.Reader:
		return &bufHndlr{v}
	default:
		buf := &bytes.Buffer{}
		io.Copy(buf, in)
		r := bytes.NewReader(buf.Bytes())
		return &bufHndlr{r}
	}
}

type fileHndlr struct {
	*os.File
}

func (f *fileHndlr) Size() int64 {
	st, err := f.File.Stat()
	if err != nil {
		// stat failed
		return -1
	}

	return st.Size()
}

type bufHndlr struct {
	*bytes.Reader
}

func (b *bufHndlr) Size() int64 {
	return int64(b.Reader.Len())
}
