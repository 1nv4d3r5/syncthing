package protocol

import "io"

type TestModel struct {
	data    []byte
	repo    string
	name    string
	offset  int64
	size    int
	closed  bool
	options map[string]string
}

func (t *TestModel) Index(nodeID, repo string, files []FileInfo) {
}

func (t *TestModel) IndexUpdate(nodeID, repo string, files []FileInfo) {
}

func (t *TestModel) Request(nodeID, repo, name string, offset int64, size int) ([]byte, error) {
	t.repo = repo
	t.name = name
	t.offset = offset
	t.size = size
	return t.data, nil
}

func (t *TestModel) Close(nodeID string, err error) {
	t.closed = true
}

func (t *TestModel) Options(nodeID string, options map[string]string) {
	t.options = options
}

type ErrPipe struct {
	io.PipeWriter
	written int
	max     int
	err     error
	closed  bool
}

func (e *ErrPipe) Write(data []byte) (int, error) {
	if e.closed {
		return 0, e.err
	}
	if e.written+len(data) > e.max {
		n, _ := e.PipeWriter.Write(data[:e.max-e.written])
		e.PipeWriter.CloseWithError(e.err)
		e.closed = true
		return n, e.err
	}
	return e.PipeWriter.Write(data)
}
