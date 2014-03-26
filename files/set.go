// Package files provides a set type to track local/remote files with newness checks.
package files

/*

Delete and version handling

SetLocal handles deletes. Any files currently in the local set that are
missing in the new set are marked as deleted. This is done by bumping the
version and clearing the size and blocks fields.

SetLocalNoDelete does not do this; it simply replaces the local set with that
which was given.

*/

import (
	"sync"

	"github.com/calmh/syncthing/cid"
	"github.com/calmh/syncthing/protocol"
	"github.com/calmh/syncthing/scanner"
)

type key struct {
	Name    string
	Version uint32
}

type fileRecord struct {
	Usage int
	File  scanner.File
}

type bitset uint64

func keyFor(f scanner.File) key {
	return key{
		Name:    f.Name,
		Version: f.Version,
	}
}

func (a key) newerThan(b key) bool {
	return a.Version > b.Version
}

type Set struct {
	sync.Mutex
	changes            int64
	files              map[key]fileRecord
	remoteKey          [64]map[string]key
	globalAvailability map[string]bitset
	globalKey          map[string]key
}

func NewSet() *Set {
	var m = Set{
		files:              make(map[key]fileRecord),
		globalAvailability: make(map[string]bitset),
		globalKey:          make(map[string]key),
	}
	return &m
}

func (m *Set) Update(id uint, fs []scanner.File) {
	m.Lock()
	m.update(id, fs)
	m.changes++
	m.Unlock()
}

func (m *Set) ReplaceWithDelete(id uint, fs []scanner.File) {
	if id > 63 {
		panic("Connection ID must be in the range 0 - 63 inclusive")
	}
	m.Lock()

	if len(fs) != len(m.remoteKey[cid.LocalID]) {
		// We know something changed for sure
		m.changes++
	}

	var nf = make(map[string]key, len(fs))
	for _, f := range fs {
		nf[f.Name] = keyFor(f)
	}

	// For previously existing files not in the list, add them to the list
	// with the relevant delete flags etc set.

	for _, ck := range m.remoteKey[cid.LocalID] {
		if nk, ok := nf[ck.Name]; !ok {
			cf := m.files[ck].File
			cf.Flags = protocol.FlagDeleted
			cf.Blocks = nil
			cf.Size = 0
			cf.Version++
			fs = append(fs, cf)
			m.changes++
			if debug {
				dlog.Println("deleted:", ck.Name)
			}
		} else if nk != ck {
			m.changes++
			if debug {
				dlog.Println("changed:", ck, nk)
			}
		}
	}

	m.replace(id, fs)
	m.Unlock()
}

func (m *Set) Replace(id uint, fs []scanner.File) {
	if id > 63 {
		panic("Connection ID must be in the range 0 - 63 inclusive")
	}
	m.Lock()
	m.replace(id, fs)
	m.changes++
	m.Unlock()
}

func (m *Set) Need(cid uint) []scanner.File {
	var fs []scanner.File
	m.Lock()
	for name, gk := range m.globalKey {
		if gk.newerThan(m.remoteKey[cid][name]) {
			fs = append(fs, m.files[gk].File)
		}
	}
	m.Unlock()
	return fs
}

func (m *Set) Have(cid uint) []scanner.File {
	var fs []scanner.File
	m.Lock()
	for _, rk := range m.remoteKey[cid] {
		fs = append(fs, m.files[rk].File)
	}
	m.Unlock()
	return fs
}

func (m *Set) Global() []scanner.File {
	var fs []scanner.File
	m.Lock()
	for _, rk := range m.globalKey {
		fs = append(fs, m.files[rk].File)
	}
	m.Unlock()
	return fs
}

func (m *Set) Get(cid uint, file string) scanner.File {
	m.Lock()
	defer m.Unlock()
	return m.files[m.remoteKey[cid][file]].File
}

func (m *Set) GetGlobal(file string) scanner.File {
	m.Lock()
	defer m.Unlock()
	return m.files[m.globalKey[file]].File
}

func (m *Set) Availability(name string) bitset {
	m.Lock()
	defer m.Unlock()
	return m.globalAvailability[name]
}

func (m *Set) Changes() int64 {
	m.Lock()
	defer m.Unlock()
	return m.changes
}

func (m *Set) update(cid uint, fs []scanner.File) {
	remFiles := m.remoteKey[cid]
	for _, f := range fs {
		n := f.Name
		fk := keyFor(f)

		if ck, ok := remFiles[n]; ok && ck == fk {
			// The remote already has exactly this file, skip it
			continue
		}

		remFiles[n] = fk

		// Keep the block list or increment the usage
		if br, ok := m.files[fk]; !ok {
			m.files[fk] = fileRecord{
				Usage: 1,
				File:  f,
			}
		} else {
			br.Usage++
			m.files[fk] = br
		}

		// Update global view
		gk, ok := m.globalKey[n]
		switch {
		case ok && fk == gk:
			av := m.globalAvailability[n]
			av |= 1 << cid
			m.globalAvailability[n] = av
		case fk.newerThan(gk):
			m.globalKey[n] = fk
			m.globalAvailability[n] = 1 << cid
		}
	}
}

func (m *Set) replace(cid uint, fs []scanner.File) {
	// Decrement usage for all files belonging to this remote, and remove
	// those that are no longer needed.
	for _, fk := range m.remoteKey[cid] {
		br, ok := m.files[fk]
		switch {
		case ok && br.Usage == 1:
			delete(m.files, fk)
		case ok && br.Usage > 1:
			br.Usage--
			m.files[fk] = br
		}
	}

	// Clear existing remote remoteKey
	m.remoteKey[cid] = make(map[string]key)

	// Recalculate global based on all remaining remoteKey
	for n := range m.globalKey {
		var nk key    // newest key
		var na bitset // newest availability

		for i, rem := range m.remoteKey {
			if rk, ok := rem[n]; ok {
				switch {
				case rk == nk:
					na |= 1 << uint(i)
				case rk.newerThan(nk):
					nk = rk
					na = 1 << uint(i)
				}
			}
		}

		if na != 0 {
			// Someone had the file
			m.globalKey[n] = nk
			m.globalAvailability[n] = na
		} else {
			// Noone had the file
			delete(m.globalKey, n)
			delete(m.globalAvailability, n)
		}
	}

	// Add new remote remoteKey to the mix
	m.update(cid, fs)
}