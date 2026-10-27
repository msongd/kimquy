// Package model knows the embedding model semd runs: EmbeddingGemma 2 in
// the layout of the EmbeddingGemma2Swift runtime, pinned to one revision of
// its Hugging Face repository. It finds the model, checks it, downloads it,
// and imports a copy the user already has.
//
// A model directory holds one folder per part (text-q8, vision-q8,
// audio-bf16). semd-embed takes a part to be there when its manifest.json
// is, so a part only ever appears whole: it is assembled in a staging
// folder, verified, and renamed into place.
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
	Repo     = "Nurymanau/EmbeddingGemma-2-MLX-Swift"
	Revision = "06b4e075e6b3772e3c29df9f2a2ace7bd4950bdd"
	// BaseURL is where a part's files are downloaded from: BaseURL/<part>/<file>.
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

// Parts are the model's parts, text first. The sizes and hashes are those
// of Revision; semd-embed derives the model id from the manifests', so any
// other weights would make a different index.
var Parts = []Part{
	{Dir: "text-q8", Label: "Text and documents", Required: true, Files: []File{
		{"config.json", 2359, "e090bf087c627155c55b481fca9d216461bdaf44d5a980e018ed3deb92fc7664"},
		{"config_sentence_transformers.json", 1565, "031e56a498d33c349ab489a21885bcfe25b4fcba841149dc99e1e90d4a7c28f5"},
		{"quantization.json", 16288, "9de7e788a89f36a07387c9758dd187b596675bfe9e0dbf9e56c3a09f1f410965"},
		{"tokenizer.json", 32170510, "4d777ef5bdc1aa36227abdfb77c3e49e7b9c892d16e1b6bda41c393504828be4"},
		{"tokenizer_config.json", 1599, "17bd5d6e9364ca49a534e1502076593317c298d4a663623091ed45388f004874"},
		{"model.safetensors", 288118885, "eb8a067456a3cff750bebadc7b985420444e673508a2da5bcf56e726e52d62c9"},
		{"manifest.json", 379, "d9cdb30351293694b7343078970c72b3696eb12034ff06bff2af663fdc3c5cd8"},
	}},
	{Dir: "vision-q8", Label: "Images, PDFs and video frames", Files: []File{
		{"config.json", 3453, "6ece510cf4df3d5370011ea0e856486f971dbd8019f5587c6a059977d61571d4"},
		{"quantization.json", 11258, "19c4f4d59a942f0829b4c5b6d8da12e24c5a5306434a22f0705e10ff2ced1186"},
		{"vision.safetensors", 193095171, "9b18ee38272989696737fe3a0c4302dfa06f47cc37192b1b8f7583f8447da173"},
		{"manifest.json", 380, "db92014f981b36cde052bc4759929d343d30b22f7888c623375b50427ffa11a4"},
	}},
	{Dir: "audio-bf16", Label: "Audio and the sound of videos", Files: []File{
		{"config.json", 3361, "500326bb2a9b4372db5fa505298f046cff27a5f4961c02102a5f8bccdebe6eac"},
		{"audio.safetensors", 611314600, "a163c6dc2e47cb894a02fb7f8e6e4d7cc4edbd81a80a8506438fd42610a526e3"},
		{"manifest.json", 196, "39cb724fbd90a0b1b99ea64a643556e73fd18444f4b01a9b9fb835fdabd3ca5e"},
	}},
}

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
