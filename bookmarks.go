package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
)

type Bookmark struct {
	Name string `json:"name"`
	Host string `json:"host"`
	Path string `json:"path"`
}

type Bookmarks struct {
	path string
	List []Bookmark `json:"bookmarks"`
}

func bookmarksPath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = filepath.Join(os.Getenv("HOME"), ".config")
	}
	return filepath.Join(dir, "bsftp", "bookmarks.json")
}

func LoadBookmarks() (*Bookmarks, error) {
	p := bookmarksPath()
	b := &Bookmarks{path: p}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return b, nil
		}
		return nil, err
	}
	if len(data) == 0 {
		return b, nil
	}
	if err := json.Unmarshal(data, b); err != nil {
		return nil, err
	}
	return b, nil
}

func (b *Bookmarks) Save() error {
	if err := os.MkdirAll(filepath.Dir(b.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(b.path, data, 0o644)
}

func (b *Bookmarks) ForHost(host string) []Bookmark {
	var out []Bookmark
	for _, x := range b.List {
		if x.Host == host {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func (b *Bookmarks) Add(name, host, path string) {
	// Replace if same (host,name) exists
	for i, x := range b.List {
		if x.Host == host && x.Name == name {
			b.List[i].Path = path
			return
		}
	}
	b.List = append(b.List, Bookmark{Name: name, Host: host, Path: path})
}

func (b *Bookmarks) Remove(host, name string) bool {
	for i, x := range b.List {
		if x.Host == host && x.Name == name {
			b.List = append(b.List[:i], b.List[i+1:]...)
			return true
		}
	}
	return false
}
