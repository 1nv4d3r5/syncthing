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
		id := cm.Get(node)
		if id == cid.LocalID {
			continue
		}
		usage := m[node]
		if availability&(1<<id) != 0 {
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

type puller struct {
	repo              string
	dir               string
	bq                *blockQueue
	model             *Model
	oustandingPerNode activityMap
	openFiles         map[string]openFile
	requestSlots      chan bool
	blocks            chan bqBlock
	requestResults    chan requestResult
}

func newPuller(repo, dir string, model *Model, slots int) *puller {
	p := &puller{
		repo:              repo,
		dir:               dir,
		bq:                newBlockQueue(),
		model:             model,
		oustandingPerNode: make(activityMap),
		openFiles:         make(map[string]openFile),
		requestSlots:      make(chan bool, slots),
		blocks:            make(chan bqBlock),
		requestResults:    make(chan requestResult),
	}
	for i := 0; i < slots; i++ {
		p.requestSlots <- true
	}
	if debugPull {
		dlog.Printf("starting puller; repo %q dir %q slots %d", repo, dir, slots)
	}
	go p.run()
	return p
}

func (p *puller) run() {
	go func() {
		// fill blocks queue when there are free slots
		for {
			<-p.requestSlots
			b := p.bq.get()
			if debugPull {
				dlog.Printf("filler: queueing %q offset %d copy %d", b.file.Name, b.block.Offset, len(b.copy))
			}
			p.blocks <- b
		}
	}()

	needTicker := time.Tick(5 * time.Second)

	for {
		select {
		case res := <-p.requestResults:
			p.oustandingPerNode.decrease(res.node)
			p.handleRequestResult(res)

		case b := <-p.blocks:
			p.handleBlock(b)

		case <-needTicker:
			if len(p.openFiles) != 0 {
				if debugNeed || debugPull {
					dlog.Printf("need: idle but have open files, not queueing more blocks\n  %#v", p.openFiles)
				}
			} else {
				p.queueNeededBlocks()
			}
		}
	}
}

func (p *puller) handleRequestResult(res requestResult) {
	of, ok := p.openFiles[res.file.Name]
	if !ok || of.err != nil {
		// no entry in openFiles means there was an error and we've cancelled the operation
		return
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
		delete(p.openFiles, res.file.Name)
		// TODO: Hash check
		t := time.Unix(res.file.Modified, 0)
		os.Chtimes(of.temp, t, t)
		os.Chmod(of.temp, os.FileMode(res.file.Flags&0777))
		os.Rename(of.temp, of.path)
		p.model.fs.Update(cid.LocalID, []scanner.File{res.file})
	}

	p.openFiles[res.file.Name] = of
}

func (p *puller) handleBlock(b bqBlock) {
	// Every path out from here must put a slot back in requestSlots

	f := b.file

	of, ok := p.openFiles[f.Name]
	if !ok {
		if debugPull {
			dlog.Printf("pull: opening file %q", f.Name)
		}
		of.path = FSNormalize(path.Join(p.dir, f.Name))
		of.temp = FSNormalize(path.Join(p.dir, defTempNamer.TempName(f.Name)))

		dirName := path.Dir(of.path)
		_, err := os.Stat(dirName)
		if err != nil {
			os.MkdirAll(dirName, 0777)
		}

		of.file, of.err = os.Create(of.temp)
		if of.err != nil {
			if debugPull {
				dlog.Printf("pull: %q: %v", f.Name, of.err)
			}
			p.openFiles[f.Name] = of
			p.requestSlots <- true
			return
		}
	}

	if of.err != nil {
		// We have already failed this file.
		if debugPull {
			dlog.Printf("pull: file %q has already failed: %v", f.Name, of.err)
		}
		if b.last {
			delete(p.openFiles, f.Name)
		}

		p.requestSlots <- true
		return
	}

	of.availability = uint64(p.model.fs.Availability(f.Name))
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

			p.openFiles[f.Name] = of
			p.requestSlots <- true
			return
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

				p.openFiles[f.Name] = of
				p.requestSlots <- true
				return
			}
		}

		exfd.Close()

	case b.block.Size > 0:
		// We have a block to get from the network

		node := p.oustandingPerNode.leastBusyNode(of.availability, p.model.cm)
		if len(node) == 0 {
			// There was no node available
			p.requestSlots <- true
			return
		}

		of.outstanding++
		p.openFiles[f.Name] = of

		go func(node string, b bqBlock) {
			// TODO: what of locking here?
			if debugPull {
				dlog.Printf("pull: requesting %q offset %d size %d from %q outstanding %d", f.Name, b.block.Offset, b.block.Size, node, of.outstanding)
			}

			p.model.pmut.Lock()
			c, ok := p.model.protoConn[node]
			p.model.pmut.Unlock()
			if !ok {
				panic("wanted request from nonexistant node " + node)
			}

			bs, err := c.Request(p.repo, f.Name, b.block.Offset, int(b.block.Size))
			p.requestResults <- requestResult{
				node:   node,
				file:   f,
				path:   of.path,
				offset: b.block.Offset,
				data:   bs,
				err:    err,
			}
			p.requestSlots <- true
		}(node, b)

	default:
		if b.last {
			if of.err == nil {
				of.file.Close()
			}
		}
		if f.Flags&protocol.FlagDeleted != 0 {
			os.Remove(of.temp)
			os.Remove(of.path)
		} else {
			if debugPull {
				dlog.Printf("pull: no blocks to fetch and nothing to copy for %q", f.Name)
			}
			t := time.Unix(f.Modified, 0)
			os.Chtimes(of.temp, t, t)
			os.Chmod(of.temp, os.FileMode(f.Flags&0777))
			os.Rename(of.temp, of.path)
		}
		p.model.fs.Update(cid.LocalID, []scanner.File{f})
		p.requestSlots <- true
	}
}

func (p *puller) queueNeededBlocks() {
	for _, f := range p.model.fs.Need(cid.LocalID) {
		lf := p.model.fs.Get(cid.LocalID, f.Name)
		have, need := scanner.BlockDiff(lf.Blocks, f.Blocks)
		if debugNeed {
			dlog.Printf("need:\n  local: %v\n  global: %v\n  haveBlocks: %v\n  needBlocks: %v", lf, f, have, need)
		}
		p.bq.put(bqAdd{
			file: f,
			have: have,
			need: need,
		})
	}
}
