package store

import (
	"io/fs"

	"github.com/pressly/goose/v3/lock"
)

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic(err) // embedded path is a compile-time constant
	}
	return sub
}

func mustLocker() lock.SessionLocker {
	l, err := lock.NewPostgresSessionLocker()
	if err != nil {
		panic(err) // only fails on invalid options, none are passed
	}
	return l
}
