package main

import (
	"os"
	"path"
	"time"

	"github.com/calmh/syncthing/buffers"
	"github.com/calmh/syncthing/cid"
	"github.com/calmh/syncthing/protocol"
	"github.com/calmh/syncthing/scanner"
)

type requestResult struct {
	node   string
	file   scanner.File
	path   string // full path name, fs-normalized
	offset int64
	data   []byte
	err    error
}

type openFile struct {
	path         string // full path name, fs-normalized
	temp         string // temporary filename, full path, fs-normalized
	availability uint64 // availability bitset
	file         *os.File
	err          error // error when opening or writing to file, all following operations are cancelled
	outstanding  int   // number of requests we still have outstanding
	done         bool  // we have sent all requests for this file
}

type activityMap map[string]int

func (m activityMap) leastBusyNode(availability uint64, cm *cid.Map) string {
	var low int = 2<<31 - 1
	var selected string
	for _, node := range cm.Names() {
		usage := m[node]
		if availability&(1<<cm.Get(node)) != 0 {
			if usage < low {
				low = usage
				selected = node
			}
		}
	}
	m[selected]++
	return selected
}

func (m activityMap) decrease(node string) {
	m[node]--
}

func (m *Model) puller(repo string, dir string, slots int) {
	var oustandingPerNode = make(activityMap)
	var openFiles = make(map[string]openFile)
	var requestSlots = make(chan bool, slots)
	var blocks = make(chan bqBlock)
	var requestResults = make(chan requestResult)

	if debugPull {
		dlog.Printf("starting puller; repo %q dir %q slots %d", repo, dir, slots)
	}

	for i := 0; i < slots; i++ {
		requestSlots <- true
	}

	go func() {
		// fill blocks queue when there are free slots
		for {
			<-requestSlots
			b := m.bq.get()
			if debugPull {
				dlog.Printf("filler: queueing %q offset %d copy %d", b.file.Name, b.block.Offset, len(b.copy))
			}
			blocks <- b
		}
	}()

pull:
	for {
		select {
		case res := <-requestResults:
			oustandingPerNode.decrease(res.node)

			of, ok := openFiles[res.file.Name]
			if !ok || of.err != nil {
				// no entry in openFiles means there was an error and we've cancelled the operation
				continue
			}

			_, of.err = of.file.WriteAt(res.data, res.offset)
			buffers.Put(res.data)
			of.outstanding--

			if debugPull {
				dlog.Printf("pull: wrote %q offset %d outstanding %d done %v", res.file, res.offset, of.outstanding, of.done)
			}

			if of.done && of.outstanding == 0 {
				if debugPull {
					dlog.Printf("pull: closing %q", res.file.Name)
				}
				of.file.Close()
				delete(openFiles, res.file.Name)
				// TODO: Hash check
				os.Rename(of.temp, of.path)
				t := time.Unix(res.file.Modified, 0)
				os.Chtimes(of.temp, t, t)
				m.fs.AddLocal([]scanner.File{res.file})
			}

			openFiles[res.file.Name] = of

		case b := <-blocks:
			// Every path out from here must put a slot back in requestSlots

			f := b.file

			of, ok := openFiles[f.Name]
			if !ok {
				if debugPull {
					dlog.Printf("pull: opening file %q", f.Name)
				}
				of.path = FSNormalize(path.Join(dir, f.Name))
				of.temp = FSNormalize(path.Join(dir, defTempNamer.TempName(f.Name)))

				dirName := path.Base(of.path)
				_, err := os.Stat(dirName)
				if err != nil {
					os.MkdirAll(dirName, 0777)
				}

				of.file, of.err = os.OpenFile(of.temp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, os.FileMode(f.Flags&0777))
				if of.err != nil {
					if debugPull {
						dlog.Printf("pull: %q: %v", f.Name, of.err)
					}
					openFiles[f.Name] = of
					requestSlots <- true
					continue pull
				}
			}

			if of.err != nil {
				// We have already failed this file.
				if debugPull {
					dlog.Printf("pull: file %q has already failed: %v", f.Name, of.err)
				}
				if b.last {
					delete(openFiles, f.Name)
				}

				requestSlots <- true
				continue pull
			}

			of.availability = uint64(m.fs.Availability(f.Name))
			of.done = b.last

			switch {
			case len(b.copy) > 0:
				// We have blocks to copy from the existing file

				if debugPull {
					dlog.Printf("pull: copying %d blocks for %q", len(b.copy), f.Name)
				}

				var exfd *os.File
				exfd, of.err = os.Open(of.path)
				if of.err != nil {
					if debugPull {
						dlog.Printf("pull: %q: %v", f.Name, of.err)
					}
					of.file.Close()

					openFiles[f.Name] = of
					requestSlots <- true
					continue pull
				}

				for _, b := range b.copy {
					bs := buffers.Get(int(b.Size))
					_, of.err = exfd.ReadAt(bs, b.Offset)
					if of.err == nil {
						_, of.err = of.file.WriteAt(bs, b.Offset)
					}
					buffers.Put(bs)
					if of.err != nil {
						if debugPull {
							dlog.Printf("pull: %q: %v", f.Name, of.err)
						}
						exfd.Close()
						of.file.Close()

						openFiles[f.Name] = of
						requestSlots <- true
						continue pull
					}
				}

				exfd.Close()

			case b.block.Size > 0:
				// We have a block to get from the network

				of.outstanding++
				openFiles[f.Name] = of

				node := oustandingPerNode.leastBusyNode(of.availability, m.cm)
				go func(node string, b bqBlock) {
					// TODO: what of locking here?
					if debugPull {
						dlog.Printf("pull: requesting %q offset %d size %d from %q outstanding %d", f.Name, b.block.Offset, b.block.Size, node, of.outstanding)
					}

					bs, err := m.protoConn[node].Request(repo, f.Name, b.block.Offset, int(b.block.Size))
					requestResults <- requestResult{
						node:   node,
						file:   f,
						path:   of.path,
						offset: b.block.Offset,
						data:   bs,
						err:    err,
					}
					requestSlots <- true
				}(node, b)

			default:
				if f.Flags&protocol.FlagDeleted != 0 {

				}
			}
		}
	}
}
