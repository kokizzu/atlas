// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

package migrate

import (
	"archive/tar"
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"
)

type (
	// Dir wraps the functionality used to interact with a migration directory.
	Dir interface {
		fs.FS
		// WriteFile writes the data to the named file.
		WriteFile(string, []byte) error

		// Files returns a set of files stored in this Dir to be executed on a database.
		Files() ([]File, error)

		// Checksum returns a HashFile of the migration directory.
		Checksum() (HashFile, error)
	}

	// File represents a single migration file.
	File interface {
		// Name returns the name of the migration file.
		Name() string
		// Desc returns the description of the migration File.
		Desc() string
		// Version returns the version of the migration File.
		Version() string
		// Bytes returns the read content of the file.
		Bytes() []byte
		// Stmts returns the set of SQL statements this file holds.
		Stmts() ([]string, error)
		// StmtDecls returns the set of SQL statements this file holds alongside its preceding comments.
		StmtDecls() ([]*Stmt, error)
	}

	// Formatter wraps the Format method.
	Formatter interface {
		// Format formats the given Plan into one or more migration files.
		Format(*Plan) ([]File, error)
	}
)

type (
	// CheckpointDir wraps the functionality used to interact
	// with a migration directory that support checkpoints.
	CheckpointDir interface {
		Dir
		// WriteCheckpoint writes the given checkpoint file to the migration directory.
		WriteCheckpoint(name, tag string, content []byte) error

		// CheckpointFiles returns a set of checkpoint files stored in this Dir,
		// ordered by name.
		CheckpointFiles() ([]File, error)

		// FilesFromCheckpoint returns the files to be executed on a database from
		// the given checkpoint file, including it. An ErrCheckpointNotFound if the
		// checkpoint file is not found in the directory.
		FilesFromCheckpoint(string) ([]File, error)
	}

	// CheckpointFile wraps the functionality used to interact with files
	// returned from a CheckpointDir.
	CheckpointFile interface {
		File
		// IsCheckpoint returns true if the file is a checkpoint file.
		IsCheckpoint() bool

		// CheckpointTag returns the tag of the checkpoint file, if defined. The tag
		// can be derived from the file name, internal metadata or directive comments.
		//
		// An ErrNotCheckpoint is returned if the file is not a checkpoint file.
		CheckpointTag() (string, error)
	}
)

var (
	// ErrNotCheckpoint is returned when calling CheckpointFile methods on a non-checkpoint file.
	ErrNotCheckpoint = errors.New("not a checkpoint file")
	// ErrCheckpointNotFound is returned when a checkpoint file is not found in the directory.
	ErrCheckpointNotFound = errors.New("no checkpoint found")
)

// LocalDir implements Dir for a local migration
// directory with default Atlas formatting.
type LocalDir struct {
	path string
}

var _ CheckpointDir = (*LocalDir)(nil)

// NewLocalDir returns a new the Dir used by a Planner to work on the given local path.
func NewLocalDir(path string) (*LocalDir, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("sql/migrate: %w", err)
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("sql/migrate: %q is not a dir", path)
	}
	return &LocalDir{path: path}, nil
}

// Path returns the local path used for opening this dir.
func (d *LocalDir) Path() string {
	return d.path
}

// Open implements fs.FS.
func (d *LocalDir) Open(name string) (fs.File, error) {
	f, err := os.Open(filepath.Join(d.path, name))
	if err != nil {
		return nil, err
	}
	return f, nil
}

// WriteFile implements Dir.WriteFile.
func (d *LocalDir) WriteFile(name string, b []byte) error {
	return os.WriteFile(filepath.Join(d.path, name), b, 0644)
}

// Files implements Dir.Files. It looks for all files with .sql suffix and orders them by filename.
func (d *LocalDir) Files() ([]File, error) {
	names, err := fs.Glob(d, "*.sql")
	if err != nil {
		return nil, err
	}
	// Sort files lexicographically.
	sort.Slice(names, func(i, j int) bool {
		return names[i] < names[j]
	})
	files := make([]File, 0, len(names))
	for _, n := range names {
		b, err := fs.ReadFile(d, n)
		if err != nil {
			return nil, fmt.Errorf("sql/migrate: read file %q: %w", n, err)
		}
		files = append(files, NewLocalFile(n, b))
	}
	return files, nil
}

// Checksum implements Dir.Checksum. By default, it calls Files() and creates a checksum from them.
func (d *LocalDir) Checksum() (HashFile, error) {
	files, err := d.Files()
	if err != nil {
		return nil, err
	}
	return NewHashFile(files)
}

// WriteCheckpoint is like WriteFile, but marks the file as a checkpoint file.
func (d *LocalDir) WriteCheckpoint(name, tag string, b []byte) error {
	var (
		args []string
		f    = NewLocalFile(name, b)
	)
	if tag != "" {
		args = append(args, tag)
	}
	f.AddDirective(directiveCheckpoint, args...)
	return d.WriteFile(name, f.Bytes())
}

// CheckpointFiles implements CheckpointDir.CheckpointFiles.
func (d *LocalDir) CheckpointFiles() ([]File, error) {
	return checkpointFiles(d)
}

// FilesFromCheckpoint implements CheckpointDir.FilesFromCheckpoint.
func (d *LocalDir) FilesFromCheckpoint(name string) ([]File, error) {
	return filesFromCheckpoint(d, name)
}

// LocalFile is used by LocalDir to implement the Scanner interface.
type LocalFile struct {
	n string
	b []byte
}

var _ CheckpointFile = (*LocalFile)(nil)

// NewLocalFile returns a new local file.
func NewLocalFile(name string, data []byte) *LocalFile {
	return &LocalFile{n: name, b: data}
}

// Name implements File.Name.
func (f *LocalFile) Name() string {
	return f.n
}

// Desc implements File.Desc.
func (f *LocalFile) Desc() string {
	parts := strings.SplitN(f.n, "_", 2)
	if len(parts) == 1 {
		return ""
	}
	return strings.TrimSuffix(parts[1], ".sql")
}

// Version implements File.Version.
func (f *LocalFile) Version() string {
	return strings.SplitN(strings.TrimSuffix(f.n, ".sql"), "_", 2)[0]
}

// Stmts returns the SQL statement exists in the local file.
func (f *LocalFile) Stmts() ([]string, error) {
	s, err := Stmts(string(f.b))
	if err != nil {
		return nil, err
	}
	stmts := make([]string, len(s))
	for i := range s {
		stmts[i] = s[i].Text
	}
	return stmts, nil
}

// StmtDecls returns the all statement declarations exist in the local file.
func (f *LocalFile) StmtDecls() ([]*Stmt, error) {
	return Stmts(string(f.b))
}

// Bytes returns local file data.
func (f *LocalFile) Bytes() []byte {
	return f.b
}

// isCheckpoint reports whether the file is a checkpoint file.
func (f *LocalFile) isCheckpoint() bool {
	return len(f.Directive(directiveCheckpoint)) > 0
}

// checkpointTag returns the tag of the checkpoint file, if defined.
func (f *LocalFile) checkpointTag() (string, error) {
	ds := f.Directive(directiveCheckpoint)
	if len(ds) == 0 {
		return "", ErrNotCheckpoint
	}
	return ds[0], nil
}

const (
	// atlas:sum directive.
	directiveSum  = "sum"
	sumModeIgnore = "ignore"
	// atlas:delimiter directive.
	directiveDelimiter = "delimiter"
	// atlas:checkpoint directive.
	directiveCheckpoint = "checkpoint"
	directivePrefixSQL  = "-- "
)

var reDirective = regexp.MustCompile(`^([ -~]*)atlas:(\w+)(?: +([ -~]*))*`)

// directive searches in the content a line that matches a directive
// with the given prefix and name. For example:
//
//	directive(c, "delimiter", "-- ")	// '-- atlas:delimiter.*'
//	directive(c, "sum", "")				// 'atlas:sum.*'
//	directive(c, "sum")					// '.*atlas:sum'
func directive(content, name string, prefix ...string) (string, bool) {
	m := reDirective.FindStringSubmatch(content)
	// In case the prefix was provided ensures it is matched.
	if len(m) == 4 && m[2] == name && (len(prefix) == 0 || prefix[0] == m[1]) {
		return m[3], true
	}
	return "", false
}

// parseDirective returns the directive name and its arguments.
// Empty strings are returned if the directive is invalid.
func parseDirective(content string) (string, string) {
	m := reDirective.FindStringSubmatch(content)
	if len(m) != 4 {
		return "", ""
	}
	return m[2], m[3]
}

// Directive returns the (global) file directives that match the provided name.
// File directives are located at the top of the file and should not be associated with any
// statement. Hence, double new lines are used to separate file directives from its content.
func (f *LocalFile) Directive(name string) (ds []string) {
	for _, c := range f.comments() {
		if d, ok := directive(c, name); ok {
			ds = append(ds, d)
		}
	}
	return ds
}

// AddDirective adds a new directive to the file.
func (f *LocalFile) AddDirective(name string, args ...string) {
	var b strings.Builder
	b.WriteString("-- atlas:" + name)
	if len(args) > 0 {
		b.WriteByte(' ')
		b.WriteString(strings.Join(args, " "))
	}
	b.WriteByte('\n')
	if len(f.comments()) == 0 {
		b.WriteByte('\n')
	}
	f.b = append([]byte(b.String()), f.b...)
}

func (f *LocalFile) comments() []string {
	var (
		comments []string
		content  = string(f.b)
	)
	for strings.HasPrefix(content, "#") || strings.HasPrefix(content, "--") {
		idx := strings.IndexByte(content, '\n')
		if idx == -1 {
			// Comments-only file.
			comments = append(comments, content)
			break
		}
		comments = append(comments, strings.TrimSpace(content[:idx]))
		content = content[idx+1:]
	}
	// File comments are separated by double newlines from file content (detached from actual statements).
	// However, if the file does not contain any statements (only comments, such as atlas:import) then the
	// collected comments should treat as file comments.
	if !strings.HasPrefix(strings.TrimLeft(content, " \t"), "\n") && content != "" {
		return nil
	}
	return comments
}

type (
	// MemDir provides an in-memory Dir implementation.
	MemDir struct {
		fs     map[string]*LocalFile
		syncTo []func(string, []byte) error
		// Optional path for the MemDir set by its creator.
		// See SetPath() and Path() methods.
		path string
	}
	// An opened MemDir.
	openedMem struct {
		dir    *MemDir
		numUse int
	}
)

var (
	// A list of the opened memory-based directories.
	memDirs struct {
		sync.Mutex
		opened map[string]*openedMem
	}
	_ CheckpointDir = (*MemDir)(nil)
)

// OpenMemDir opens an in-memory directory and registers it in the process namespace
// with the given name. Hence, calling OpenMemDir with the same name will return the
// same directory. The directory is deleted when the last reference of it is closed.
func OpenMemDir(name string) *MemDir {
	memDirs.Lock()
	defer memDirs.Unlock()
	if m, ok := memDirs.opened[name]; ok {
		m.numUse++
		return m.dir
	}
	if memDirs.opened == nil {
		memDirs.opened = make(map[string]*openedMem)
	}
	memDirs.opened[name] = &openedMem{dir: &MemDir{}, numUse: 1}
	return memDirs.opened[name].dir
}

// Open implements fs.FS.
func (d *MemDir) Open(name string) (fs.File, error) {
	f, ok := d.fs[name]
	if !ok {
		return nil, fs.ErrNotExist
	}
	return &memFile{
		ReadCloser: io.NopCloser(bytes.NewReader(f.Bytes())),
	}, nil
}

// Reset the in-memory directory to its initial state.
func (d *MemDir) Reset() {
	d.fs = nil
	d.syncTo = nil
}

// Close implements the io.Closer interface.
func (d *MemDir) Close() error {
	memDirs.Lock()
	defer memDirs.Unlock()
	var opened string
	for name, m := range memDirs.opened {
		switch {
		case m.dir != d:
		case opened != "":
			return fmt.Errorf("dir was opened with different names: %q and %q", opened, name)
		default:
			opened = name
			if m.numUse--; m.numUse == 0 {
				delete(memDirs.opened, name)
			}
		}
	}
	return nil
}

// WriteFile adds a new file in-memory.
func (d *MemDir) WriteFile(name string, data []byte) error {
	if d.fs == nil {
		d.fs = make(map[string]*LocalFile)
	}
	d.fs[name] = NewLocalFile(name, data)
	for _, f := range d.syncTo {
		if err := f(name, data); err != nil {
			return err
		}
	}
	return nil
}

// CopyFiles copies all files to the MemDir and update the
// atlas.sum file, or create it if it does not exist.
func (d *MemDir) CopyFiles(fs []File) error {
	for _, f := range fs {
		if err := d.WriteFile(f.Name(), f.Bytes()); err != nil {
			return err
		}
	}
	sum, err := NewHashFile(fs)
	if err != nil {
		return err
	}
	return WriteSumFile(d, sum)
}

// WriteCheckpoint is like WriteFile, but marks the file as a checkpoint file.
func (d *MemDir) WriteCheckpoint(name, tag string, b []byte) error {
	var (
		args []string
		f    = NewLocalFile(name, b)
	)
	if tag != "" {
		args = append(args, tag)
	}
	f.AddDirective(directiveCheckpoint, args...)
	return d.WriteFile(name, f.Bytes())
}

// CheckpointFiles implements CheckpointDir.CheckpointFiles.
func (d *MemDir) CheckpointFiles() ([]File, error) {
	return checkpointFiles(d)
}

// FilesFromCheckpoint implements CheckpointDir.FilesFromCheckpoint.
func (d *MemDir) FilesFromCheckpoint(name string) ([]File, error) {
	return filesFromCheckpoint(d, name)
}

// SyncWrites allows syncing writes from in-memory directory
// the underlying storage.
func (d *MemDir) SyncWrites(fs ...func(string, []byte) error) {
	d.syncTo = append(d.syncTo, fs...)
}

// Files returns a set of files stored in-memory to be executed on a database.
func (d *MemDir) Files() ([]File, error) {
	files := make([]File, 0, len(d.fs))
	for _, f := range d.fs {
		if filepath.Ext(f.Name()) == ".sql" {
			files = append(files, f)
		}
	}
	sort.Slice(files, func(i, j int) bool {
		return files[i].Name() < files[j].Name()
	})
	return files, nil
}

// Checksum implements Dir.Checksum.
func (d *MemDir) Checksum() (HashFile, error) {
	files, err := d.Files()
	if err != nil {
		return nil, err
	}
	return NewHashFile(files)
}

// SetPath allows the caller to set a path that can be retrieved by a user using the Path method.
// Although MemDir resides only in memory, its source can be associated with a real directory
// (such as a template directory).
func (d *MemDir) SetPath(path string) {
	d.path = path
}

// Path returns the path set by the SetPath method.
func (d *MemDir) Path() string {
	return d.path
}

const versionFormat = "20060102150405"

// NewVersion generates a new migration version.
func NewVersion() string {
	return time.Now().UTC().Format(versionFormat)
}

// CheckVersion checks if the given version is valid Atlas version.
func CheckVersion(v string) error {
	if _, err := time.Parse(versionFormat, v); err != nil {
		return fmt.Errorf("invalid version: %w. Expected format is: %s", err, versionFormat)
	}
	return nil
}

// delim formats the given string as an atlas:delimiter directive.
func delim(s string) string {
	// Escape delimiters. e.g. "\n" => "\\n".
	s = strings.NewReplacer("\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(s)
	return fmt.Sprintf("-- atlas:%s %s", directiveDelimiter, s)
}

// directives returns the all directives of the plan, if exists.
func directives(p *Plan) (string, error) {
	var ds []string
	if p.Delimiter != "" {
		ds = append(ds, delim(p.Delimiter))
	}
	for _, d := range p.Directives {
		switch name, _ := parseDirective(d); {
		case name == "":
			return "", fmt.Errorf("invalid directive: %q", d)
		case name == directiveDelimiter && p.Delimiter != "":
			return "", fmt.Errorf("duplicate directive: %q. delimiter already set", d)
		}
		ds = append(ds, d)
	}
	if len(ds) == 0 {
		return "", nil
	}
	return fmt.Sprintf("%s\n\n", strings.Join(ds, "\n")), nil
}

var (
	// templateFunc contains the template.FuncMap for the DefaultFormatter.
	templateFuncs = template.FuncMap{
		"upper":      strings.ToUpper,
		"now":        NewVersion,
		"directives": directives,
	}
	// DefaultFormatter is a default implementation for Formatter.
	DefaultFormatter = TemplateFormatter{
		{
			N: template.Must(template.New("").Funcs(templateFuncs).Parse(
				"{{ with .Version }}{{ . }}{{ else }}{{ now }}{{ end }}{{ with .Name }}_{{ . }}{{ end }}.sql",
			)),
			C: template.Must(template.New("").Funcs(templateFuncs).Parse(
				`{{ directives . }}{{ range .Changes }}{{ with .Comment }}{{ printf "-- %s%s\n" (slice . 0 1 | upper ) (slice . 1) }}{{ end }}{{ printf "%s%s\n" .Cmd (or $.Delimiter ";") }}{{ end }}`,
			)),
		},
	}
)

// TemplateFormatter implements Formatter by using templates.
type TemplateFormatter []struct{ N, C *template.Template }

// NewTemplateFormatter creates a new Formatter working with the given templates.
//
//	migrate.NewTemplateFormatter(
//		template.Must(template.New("").Parse("{{now.Unix}}{{.Name}}.sql")),                 // name template
//		template.Must(template.New("").Parse("{{range .Changes}}{{println .Cmd}}{{end}}")), // content template
//	)
func NewTemplateFormatter(templates ...*template.Template) (TemplateFormatter, error) {
	if n := len(templates); n == 0 || n%2 == 1 {
		return nil, fmt.Errorf("zero or odd number of templates given: %d", n)
	}
	t := make(TemplateFormatter, 0, len(templates))
	for i := 0; i < len(templates); i += 2 {
		t = append(t, struct{ N, C *template.Template }{templates[i], templates[i+1]})
	}
	return t, nil
}

// Format implements the Formatter interface.
func (t TemplateFormatter) Format(plan *Plan) ([]File, error) {
	files := make([]File, 0, len(t))
	for _, tpl := range t {
		var n, b bytes.Buffer
		if err := tpl.N.Execute(&n, plan); err != nil {
			return nil, err
		}
		if err := tpl.C.Execute(&b, plan); err != nil {
			return nil, err
		}
		files = append(files, &LocalFile{
			n: n.String(),
			b: b.Bytes(),
		})
	}
	return files, nil
}

// FormatFile is like Format, but expects and returns a single file.
func (t TemplateFormatter) FormatFile(p *Plan) (File, error) {
	files, err := t.Format(p)
	if err != nil {
		return nil, err
	}
	if len(files) != 1 {
		return nil, fmt.Errorf("expected a single file, got %d", len(files))
	}
	return files[0], nil
}

// FormatTo calls Format and writes the files' content to the given writer.
func (t TemplateFormatter) FormatTo(plan *Plan, w io.Writer) error {
	files, err := t.Format(plan)
	if err != nil {
		return err
	}
	for _, f := range files {
		if _, err := w.Write(f.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

// HashFileName of the migration directory integrity sum file.
const HashFileName = "atlas.sum"

// HashFile represents the integrity sum file of the migration dir.
type HashFile []struct{ N, H string }

// NewHashFile computes and returns a HashFile from the given directory's files.
func NewHashFile(files []File) (HashFile, error) {
	var (
		hs HashFile
		h  = sha256.New()
	)
	for _, f := range files {
		if _, err := h.Write([]byte(f.Name())); err != nil {
			return nil, err
		}
		// Check if this file contains an "atlas:sum" directive and if so, act to it.
		if mode, ok := directive(string(f.Bytes()), directiveSum); ok && mode == sumModeIgnore {
			continue
		}
		if _, err := h.Write(f.Bytes()); err != nil {
			return nil, err
		}
		hs = append(hs, struct{ N, H string }{f.Name(), base64.StdEncoding.EncodeToString(h.Sum(nil))})
	}
	return hs, nil
}

// WriteSumFile writes the given HashFile to the Dir. If the file does not exist, it is created.
func WriteSumFile(dir Dir, sum HashFile) error {
	b, err := sum.MarshalText()
	if err != nil {
		return err
	}
	return dir.WriteFile(HashFileName, b)
}

// Sum returns the checksum of the represented hash file.
func (f HashFile) Sum() string {
	sha := sha256.New()
	for _, f := range f {
		sha.Write([]byte(f.N))
		sha.Write([]byte(f.H))
	}
	return base64.StdEncoding.EncodeToString(sha.Sum(nil))
}

// MarshalText implements encoding.TextMarshaler.
func (f HashFile) MarshalText() ([]byte, error) {
	buf := new(bytes.Buffer)
	for _, f := range f {
		fmt.Fprintf(buf, "%s h1:%s\n", f.N, f.H)
	}
	return []byte(fmt.Sprintf("h1:%s\n%s", f.Sum(), buf)), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (f *HashFile) UnmarshalText(b []byte) error {
	sc := bufio.NewScanner(bytes.NewReader(b))
	// The first line contains the sum.
	sc.Scan()
	sum := strings.TrimPrefix(sc.Text(), "h1:")
	for sc.Scan() {
		li := strings.SplitN(sc.Text(), "h1:", 2)
		if len(li) != 2 {
			return ErrChecksumFormat
		}
		*f = append(*f, struct{ N, H string }{strings.TrimSpace(li[0]), li[1]})
	}
	if sum != f.Sum() {
		return ErrChecksumMismatch
	}
	return sc.Err()
}

// SumByName returns the hash for a migration file by its name.
func (f HashFile) SumByName(n string) (string, error) {
	for _, f := range f {
		if f.N == n {
			return f.H, nil
		}
	}
	return "", errors.New("checksum not found")
}

// Reason for a checksum mismatch.
const (
	ReasonAdded Reason = iota + 1
	ReasonEdited
	ReasonRemoved
)

var (
	// ErrChecksumFormat is returned from Validate if the sum files format is invalid.
	ErrChecksumFormat = errors.New("checksum file format invalid")
	// ErrChecksumMismatch is returned when unmarshalling from a sum file and the files sum entries don't match.
	ErrChecksumMismatch = errors.New("checksum mismatch")
	// ErrChecksumNotFound is returned from Validate if the hash file does not exist.
	ErrChecksumNotFound = errors.New("checksum file not found")
)

type (
	// ChecksumError indicates a mismatch between a directories files and its sum.
	ChecksumError struct {
		Line, Total, Pos int    // line number in file of the mismatch, total number of lines, pos in file
		File             string // filename of the mismatch
		Reason           Reason // reason of a mismatch by filename
	}
	// Reason for a checksum mismatch.
	Reason uint
)

// Error implements the error interface.
func (err *ChecksumError) Error() string { return ErrChecksumMismatch.Error() }

// Is exists for backwards compatability reasons.
func (err *ChecksumError) Is(target error) bool {
	return errors.Is(ErrChecksumMismatch, target)
}

// String implements fmt.Stringer.
func (r Reason) String() string {
	switch r {
	case ReasonAdded:
		return "added"
	case ReasonEdited:
		return "edited"
	case ReasonRemoved:
		return "removed"
	}
	return "unknown reason"
}

// Validate checks if the migration dir is in sync with its sum file.
// If they don't match ErrChecksumMismatch is returned.
func Validate(dir Dir) error {
	ac, err := readHashFile(dir)
	if errors.Is(err, fs.ErrNotExist) {
		// If there are no migration files yet, this is okay.
		if files, err := dir.Files(); err != nil {
			return err
		} else if len(files) > 0 {
			return ErrChecksumNotFound
		}
		return nil
	}
	if err != nil {
		return err
	}
	ex, err := dir.Checksum()
	if err != nil {
		return err
	}
	if ac.Sum() != ex.Sum() {
		err := &ChecksumError{Total: len(ac)}
		// Determine the reason for the mismatch. Iterate over the file sum,
		// based on it determine if a file was removed, added or edited.
		const hashSize = 3 + 44 // h1: (3) + base64(sha256sum) (44)
		pos := hashSize + 1     // total hash + newline
		for i, h := range ac {
			// Proceed until we find the mismatch.
			if len(ex) > i && ex[i] == h {
				pos += len(h.N) + 1 + hashSize + 1 // filename + space + hash + newline
				continue
			}
			// Index is now pointing at the file with the mismatch.
			err.Line = i + 2 // first line is global hash
			err.Pos = pos
			err.File = h.N
			switch idx := slices.IndexFunc(ex, func(e struct{ N, H string }) bool { return e.N == h.N }); {
			case idx < 0:
				err.Reason = ReasonRemoved
			case idx == i:
				// If the file is in its original place, it was edited.
				err.Reason = ReasonEdited
			default:
				// File was not in its original place, meaning another file was added before it.
				err.File = ex[i].N
				err.Reason = ReasonAdded
			}
			return err
		}
		// If we land here, all migrations in the sum file are present unchanged in the computed sum.
		// But there is a mismatch, meaning the next file in the computed sum was added.
		err.Line = err.Total + 2 // first line is global hash
		err.File = ex[err.Total].N
		err.Pos = pos
		err.Reason = ReasonAdded
		return err
	}
	return nil
}

// FilesLastIndex returns the index of the last file
// satisfying f(i), or -1 if none do.
func FilesLastIndex[F File](files []F, f func(F) bool) int {
	for i := len(files) - 1; i >= 0; i-- {
		if f(files[i]) {
			return i
		}
	}
	return -1
}

// SkipCheckpointFiles returns a filtered set of files that are not checkpoint files.
func SkipCheckpointFiles(all []File) []File {
	files := make([]File, 0, len(all))
	for _, f := range all {
		if ck, ok := f.(CheckpointFile); ok && ck.IsCheckpoint() {
			continue
		}
		files = append(files, f)
	}
	return files
}

// FilesFromLastCheckpoint returns a set of files created after the last checkpoint,
// if exists, to be executed on a database (on the first time). Note, if the Dir is
// not a CheckpointDir, or no checkpoint file was found, all files are returned.
func FilesFromLastCheckpoint(dir Dir) ([]File, error) {
	ck, ok := dir.(CheckpointDir)
	if !ok {
		return dir.Files()
	}
	cks, err := ck.CheckpointFiles()
	if err != nil {
		return nil, err
	}
	if len(cks) == 0 {
		return dir.Files()
	}
	return ck.FilesFromCheckpoint(cks[len(cks)-1].Name())
}

// checkpointFiles returns all checkpoint files in a migration directory.
func checkpointFiles(d Dir) ([]File, error) {
	files, err := d.Files()
	if err != nil {
		return nil, err
	}
	var cks []File
	for _, f := range files {
		if ck, ok := f.(CheckpointFile); ok && ck.IsCheckpoint() {
			cks = append(cks, f)
		}
	}
	return cks, nil
}

// filesFromCheckpoint returns all files from the given checkpoint
// to be executed on the database, including the checkpoint file.
func filesFromCheckpoint(d Dir, name string) ([]File, error) {
	files, err := d.Files()
	if err != nil {
		return nil, err
	}
	i := FilesLastIndex(files, func(f File) bool {
		c, ok := f.(CheckpointFile)
		return ok && c.IsCheckpoint() && f.Name() == name
	})
	if i == -1 {
		return nil, ErrCheckpointNotFound
	}
	return files[i:], nil
}

// readHashFile reads the HashFile from the given Dir.
func readHashFile(dir Dir) (HashFile, error) {
	f, err := dir.Open(HashFileName)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var fh HashFile
	if err := fh.UnmarshalText(b); err != nil {
		return nil, err
	}
	return fh, nil
}

// memFile implements the File interface for a file in memory.
type memFile struct{ io.ReadCloser }

// Stat returns a zero FileInfo.
func (m *memFile) Stat() (fs.FileInfo, error) { return m, nil }
func (m *memFile) Name() string               { return "" }
func (m *memFile) Size() int64                { return 0 }
func (m *memFile) Mode() fs.FileMode          { return 0 }
func (m *memFile) ModTime() time.Time         { return time.Time{} }
func (m *memFile) IsDir() bool                { return false }
func (m *memFile) Sys() interface{}           { return nil }

// ArchiveDir returns a tar archive of the given directory.
func ArchiveDir(dir Dir) ([]byte, error) {
	var buf bytes.Buffer
	if err := ArchiveDirTo(&buf, dir); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ArchiveDirTo writes a tar archive of the given directory to the given writer.
func ArchiveDirTo(w io.Writer, dir Dir) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	sumF, err := dir.Open(HashFileName)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if sumF != nil {
		sumB, err := io.ReadAll(sumF)
		if err != nil {
			return err
		}
		if err := append2Tar(tw, HashFileName, sumB); err != nil {
			return err
		}
	}
	files, err := dir.Files()
	if err != nil {
		return err
	}
	for _, f := range files {
		if err := append2Tar(tw, f.Name(), f.Bytes()); err != nil {
			return err
		}
	}
	return nil
}

// UnarchiveDir extracts the tar archive into the given directory.
func UnarchiveDir(arc []byte) (Dir, error) {
	return UnarchiveDirFrom(bytes.NewReader(arc))
}

// UnarchiveDirFrom extracts the tar archive into the given directory.
func UnarchiveDirFrom(r io.Reader) (Dir, error) {
	var (
		md = &MemDir{}
		tr = tar.NewReader(r)
	)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if err := md.WriteFile(h.Name, data); err != nil {
			return nil, err
		}
	}
	return md, nil
}

func append2Tar(tw *tar.Writer, name string, data []byte) error {
	if err := tw.WriteHeader(&tar.Header{
		Name: name,
		Mode: 0600,
		Size: int64(len(data)),
	}); err != nil {
		return err
	}
	if _, err := tw.Write(data); err != nil {
		return err
	}
	return nil
}
