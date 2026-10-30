package main

import (
	"encoding/binary"
	"os"
)

// Q2 Pod keeps its place in each podcast it plays (navigation.c's resume ring, /mnt/data/ringnav-resume):
// 512 slots of {the path's FNV-1a hash, seconds in}, little-endian, most recent first; a place within
// 30 s of either end is forgotten. An episode's row shows how far in it is.

// placeKey is Q2 Pod's key for a song's path (coverflow.c fnv over FNV_SEED, never 0).
func placeKey(path string) uint32 {
	h := uint32(2166136261)
	for i := 0; i < len(path); i++ {
		h = (h ^ uint32(path[i])) * 16777619
	}
	if h *= 16777619; h == 0 {
		h = 1
	}
	return h
}

// places is the resume ring by key, read again only when the file changes.
func (a *app) places() map[uint32]int {
	st, err := os.Stat(a.opts.resume)
	if err != nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if st.ModTime().Equal(a.placesAt) && a.placeMap != nil {
		return a.placeMap
	}
	b, err := os.ReadFile(a.opts.resume)
	if err != nil {
		return nil
	}
	m := map[uint32]int{}
	for i := 0; i+8 <= len(b); i += 8 {
		key, sec := binary.LittleEndian.Uint32(b[i:]), int32(binary.LittleEndian.Uint32(b[i+4:]))
		if _, seen := m[key]; key != 0 && sec > 0 && !seen {
			m[key] = int(sec)
		}
	}
	a.placesAt, a.placeMap = st.ModTime(), m
	return m
}
