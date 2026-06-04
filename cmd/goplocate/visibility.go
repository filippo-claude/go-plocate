// go-plocate: a read-only, pure-Go reimplementation of the plocate database
// reader. This is a derivative work of plocate by Steinar H. Gunderson
// (https://git.sesse.net/plocate); the Go port was produced by an LLM agent.
//
// Copyright 2020 Steinar H. Gunderson (original plocate, C++).
// Copyright 2026 Filippo Valsorda (Go port; written by an LLM agent).
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU General Public License as published by the Free
// Software Foundation, either version 2 of the License, or (at your option)
// any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS
// FOR A PARTICULAR PURPOSE. See the GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License along with
// this program (see the file COPYING). If not, see
// <https://www.gnu.org/licenses/>.

package main

import "golang.org/x/sys/unix"

// visibilityCache replicates plocate's AccessRXCache: a file is "visible" only
// if every ancestor directory is readable and searchable (access(dir,
// R_OK|X_OK)). Results are cached per directory.
type visibilityCache struct {
	cache map[string]bool
}

func newVisibilityCache() *visibilityCache {
	return &visibilityCache{cache: map[string]bool{}}
}

// visible reports whether filename's ancestor directories are all accessible.
// It mirrors AccessRXCache::check_access: it checks every prefix of filename
// ending at a '/', starting after the leading character.
func (v *visibilityCache) visible(filename string) bool {
	for i := 1; i < len(filename); i++ {
		if filename[i] != '/' {
			continue
		}
		parent := filename[:i]
		ok, cached := v.cache[parent]
		if cached {
			if !ok {
				return false
			}
			continue
		}
		ok = unix.Access(parent, unix.R_OK|unix.X_OK) == nil
		v.cache[parent] = ok
		if !ok {
			return false
		}
	}
	return true
}
