// Package model knows the embedding model semd runs: EmbeddingGemma 2 as a
// LiteRT-LM file, pinned to one revision of its Hugging Face repository. It
// finds the model, checks it, downloads it, and imports a copy the user
// already has.
//
// A model directory holds one folder per part (litert). A part only ever
// appears whole: it is assembled in a staging folder, verified, and renamed
// into place.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

const (
	// Repo is the Hugging Face repository of the weights, and Revision the
	// commit the files below come from.
	Repo     = "litert-community/embeddinggemma-2-740m-litert-lm"
	Revision = "94408f31843f8bd0adb11c262292627287f5fda4"
	// BaseURL is where a part's files are downloaded from:
	// BaseURL/[<remote>/]<file>.
	BaseURL = "https://huggingface.co/" + Repo + "/resolve/" + Revision
	// License is the weights' license (Google's original checkpoint and
	// this conversion alike).
	License = "Apache-2.0"
)

// File is one file of a part.
type File struct {
	Name   string
	Size   int64
	SHA256 string
}

// Part is a folder of the model: one tower of it.
type Part struct {
	Dir      string // its folder in a model directory
	Remote   string // its folder in Repo; "" for the root
	Label    string // what it is for, for people
	Required bool   // semd does not start without it
	Files    []File
}

// Size is how many bytes its files take.
func (p Part) Size() int64 {
	var n int64
	for _, f := range p.Files {
		n += f.Size
	}
	return n
}

// Parts are the model's parts, the required one first. The sizes and
// hashes are those of Revision; semd-embed (embedder/Sources/semd-embed/
// Engine.swift) pins the same file and derives the model id from its hash,
// so other weights would make a different index.
var Parts = []Part{
	{Dir: "litert", Label: "Text, documents, images, audio and video", Required: true, Files: []File{
		{"embeddinggemma-2-740m.litertlm", 484622336, "e7a8a2204b91e0f96e92960e84a09a89212e1633dcb7575a9bf3378b4df77f4c"},
	}},
}

// Obsolete are the part folders of earlier models (the MLX runtime's). The
// installer deletes them from its own directory once Parts are in.
var Obsolete = []string{"text-q8", "vision-q8", "audio-bf16"}

// PartByDir returns the part in folder dir.
func PartByDir(dir string) (Part, bool) {
	for _, p := range Parts {
		if p.Dir == dir {
			return p, true
		}
	}
	return Part{}, false
}

// Dir is where a downloaded or imported model lives in semd's state
// directory.
func Dir(state string) string { return filepath.Join(state, "models") }

// Bundled returns the model in the app bundle of the semd executable at
// semd (Kim Quy.app/Contents/Helpers/semd → Contents/Resources/models), or
// "" when there is none.
func Bundled(semd string) string {
	d := filepath.Join(filepath.Dir(filepath.Dir(semd)), "Resources", "models")
	if fi, err := os.Stat(filepath.Join(d, Parts[0].Dir)); err != nil || !fi.IsDir() {
		return ""
	}
	return d
}

// Find returns the model directory semd at path uses with state: the
// bundled one if the app has it, the one in state otherwise.
func Find(semd, state string) string {
	if d := Bundled(semd); d != "" {
		return d
	}
	return Dir(state)
}

// Check reports whether part p is in model directory dir, looking at its
// files' sizes only. Missing is false and no error; a part that is there
// but not whole (a file missing, or of the wrong size) is an error.
func Check(dir string, p Part) (bool, error) {
	pd := filepath.Join(dir, p.Dir)
	if _, err := os.Stat(pd); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	for _, f := range p.Files {
		fi, err := os.Stat(filepath.Join(pd, f.Name))
		switch {
		case err != nil:
			return false, fmt.Errorf("model: %s is incomplete: %w", pd, err)
		case !fi.Mode().IsRegular() || fi.Size() != f.Size:
			return false, fmt.Errorf("model: %s is damaged: %s is %d bytes, not %d", pd, f.Name, fi.Size(), f.Size)
		}
	}
	return true, nil
}

// Installed reports which parts dir holds, by folder name, with the first
// damaged part's error.
func Installed(dir string) (map[string]bool, error) {
	have := map[string]bool{}
	for _, p := range Parts {
		ok, err := Check(dir, p)
		if err != nil {
			return have, err
		}
		have[p.Dir] = ok
	}
	return have, nil
}

// Verify hashes every file of part p in dir against its pinned hash.
func Verify(dir string, p Part, progress func(n int64)) error {
	for _, f := range p.Files {
		if err := verifyFile(filepath.Join(dir, p.Dir, f.Name), f, progress); err != nil {
			return err
		}
	}
	return nil
}

// verifyFile hashes path and compares it with f, reporting bytes read.
func verifyFile(path string, f File, progress func(n int64)) error {
	in, err := os.Open(path)
	if err != nil {
		return err
	}
	defer in.Close()
	h := sha256.New()
	n, err := io.Copy(h, &counter{r: in, add: progress})
	if err != nil {
		return fmt.Errorf("model: read %s: %w", path, err)
	}
	if n != f.Size {
		return fmt.Errorf("model: %s is %d bytes, not %d", path, n, f.Size)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != f.SHA256 {
		return fmt.Errorf("model: %s is not the expected file (sha256 %s, want %s)", path, sum, f.SHA256)
	}
	return nil
}

// counter reports the bytes read through it.
type counter struct {
	r   io.Reader
	add func(int64)
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && c.add != nil {
		c.add(int64(n))
	}
	return n, err
}
