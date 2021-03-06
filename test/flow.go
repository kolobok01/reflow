// Copyright 2017 GRAIL, Inc. All rights reserved.
// Use of this source code is governed by the Apache 2.0
// license that can be found in the LICENSE file.

package test

import (
	"strings"

	"github.com/grailbio/reflow"
)

// The following are useful constructors for testing.

// Files returns a value comprising the given files with contents derived from
// their names.
func Files(files ...string) reflow.Fileset {
	var v reflow.Fileset
	v.Map = map[string]reflow.File{}
	for _, file := range files {
		var path, contents string
		parts := strings.SplitN(file, ":", 2)
		switch len(parts) {
		case 1:
			path = file
			contents = file
		case 2:
			path = parts[0]
			contents = parts[1]
		}
		v.Map[path] = reflow.File{
			reflow.Digester.FromString(contents),
			int64(len(contents)),
		}
	}
	return v
}

// List constructs a list value.
func List(values ...reflow.Fileset) reflow.Fileset {
	return reflow.Fileset{List: values}
}
