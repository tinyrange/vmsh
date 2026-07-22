package shell

import (
	"archive/tar"
	cryptorand "crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// hostTarExtractor owns a filesystem-root capability for the exact tree an
// archive may modify. Archive names are always interpreted relative to root;
// they are never converted back into unrestricted host paths.
type hostTarExtractor struct {
	root           *os.Root
	mode           copyDestMode
	archiveRoot    string
	exactDirectory bool
	exactTarget    string
	dirs           []hostTarDirMtime
	regulars       map[string]string
}

type hostTarDirMtime struct {
	name  string
	mtime time.Time
}

func extractTarToHostRooted(r io.Reader, dst copyTargetPath) error {
	if dst.path == "" {
		return fmt.Errorf("copy destination path is required")
	}
	tr := tar.NewReader(r)
	first, err := tr.Next()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}

	extractor, firstTarget, err := newHostTarExtractor(dst, first)
	if err != nil {
		return err
	}
	defer func() { _ = extractor.root.Close() }()

	if err := extractor.extract(firstTarget, first, tr); err != nil {
		return err
	}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return extractor.restoreDirMtimes()
		}
		if err != nil {
			return err
		}
		target, err := extractor.target(header.Name)
		if err != nil {
			return err
		}
		if err := extractor.extract(target, header, tr); err != nil {
			return err
		}
	}
}

func newHostTarExtractor(dst copyTargetPath, first *tar.Header) (*hostTarExtractor, string, error) {
	firstName, err := cleanHostTarName(first.Name)
	if err != nil {
		return nil, "", err
	}
	mode := hostCopyDestMode(dst.path, dst.directory)
	absDst, err := filepath.Abs(dst.path)
	if err != nil {
		return nil, "", fmt.Errorf("resolve copy destination: %w", err)
	}
	if mode == copyDestIntoDir {
		root, err := openHostTarDestinationRoot(absDst)
		if err != nil {
			return nil, "", err
		}
		return &hostTarExtractor{root: root, mode: mode}, firstName, nil
	}

	rootName, remainder := splitHostTarRoot(firstName)
	directoryArchive := first.Typeflag == tar.TypeDir || remainder != ""
	if directoryArchive {
		root, err := openHostTarDestinationRoot(absDst)
		if err != nil {
			return nil, "", err
		}
		target := remainder
		if target == "" {
			target = "."
		}
		return &hostTarExtractor{
			root:           root,
			mode:           mode,
			archiveRoot:    rootName,
			exactDirectory: true,
		}, target, nil
	}

	parent := filepath.Dir(absDst)
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, "", fmt.Errorf("create copy destination parent: %w", err)
	}
	root, err := os.OpenRoot(parent)
	if err != nil {
		return nil, "", fmt.Errorf("open copy destination parent: %w", err)
	}
	target := filepath.Base(absDst)
	if !filepath.IsLocal(target) {
		_ = root.Close()
		return nil, "", fmt.Errorf("unsafe copy destination %q", dst.path)
	}
	return &hostTarExtractor{
		root:        root,
		mode:        mode,
		archiveRoot: rootName,
		exactTarget: target,
	}, target, nil
}

func (e *hostTarExtractor) target(name string) (string, error) {
	clean, err := cleanHostTarName(name)
	if err != nil {
		return "", err
	}
	if e.mode == copyDestIntoDir {
		return clean, nil
	}
	rootName, remainder := splitHostTarRoot(clean)
	if rootName != e.archiveRoot {
		return "", fmt.Errorf("tar archive contains multiple roots %q and %q", e.archiveRoot, rootName)
	}
	if e.exactDirectory {
		if remainder == "" {
			return ".", nil
		}
		return remainder, nil
	}
	if remainder != "" {
		return "", fmt.Errorf("tar archive has child %q beneath non-directory root %q", name, e.archiveRoot)
	}
	return e.exactTarget, nil
}

func cleanHostTarName(name string) (string, error) {
	slashName := filepath.ToSlash(name)
	if slashName == "" || strings.ContainsRune(slashName, 0) || path.IsAbs(slashName) {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	clean := path.Clean(slashName)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	local := filepath.FromSlash(clean)
	if !filepath.IsLocal(local) {
		return "", fmt.Errorf("unsafe tar path %q", name)
	}
	return local, nil
}

func splitHostTarRoot(name string) (string, string) {
	parts := strings.SplitN(filepath.ToSlash(name), "/", 2)
	if len(parts) == 1 {
		return filepath.FromSlash(parts[0]), ""
	}
	return filepath.FromSlash(parts[0]), filepath.FromSlash(parts[1])
}

func openHostTarDestinationRoot(dst string) (*os.Root, error) {
	parent := filepath.Dir(dst)
	if parent == dst {
		info, err := os.Lstat(dst)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("copy destination %s is not a real directory", dst)
		}
		root, err := os.OpenRoot(dst)
		if err != nil {
			return nil, err
		}
		opened, err := root.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = root.Close()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("copy destination %s changed while opening", dst)
		}
		return root, nil
	}

	if err := os.MkdirAll(parent, 0o755); err != nil {
		return nil, fmt.Errorf("create copy destination parent: %w", err)
	}
	parentRoot, err := os.OpenRoot(parent)
	if err != nil {
		return nil, fmt.Errorf("open copy destination parent: %w", err)
	}
	defer func() { _ = parentRoot.Close() }()
	root, err := openHostTarDir(parentRoot, filepath.Base(dst), true, 0o755)
	if err != nil {
		return nil, fmt.Errorf("open copy destination: %w", err)
	}
	return root, nil
}

func (e *hostTarExtractor) extract(name string, header *tar.Header, tr *tar.Reader) error {
	perm := os.FileMode(header.Mode).Perm()
	switch header.Typeflag {
	case tar.TypeDir:
		dir, err := openHostTarDir(e.root, name, true, perm|0o700)
		if err != nil {
			return err
		}
		file, openErr := dir.Open(".")
		if openErr != nil {
			_ = dir.Close()
			return openErr
		}
		if err := file.Chmod(perm); err != nil {
			_ = file.Close()
			_ = dir.Close()
			return err
		}
		if err := file.Close(); err != nil {
			_ = dir.Close()
			return err
		}
		_ = dir.Close()
		e.dirs = append(e.dirs, hostTarDirMtime{name: name, mtime: header.ModTime})
		return nil
	case tar.TypeSymlink:
		return e.writeSymlink(name, header.Linkname)
	case tar.TypeLink:
		return e.writeHardlink(name, header)
	case tar.TypeReg, 0:
		if err := e.writeRegular(name, perm, header.ModTime, header, tr); err != nil {
			return err
		}
		if e.regulars == nil {
			e.regulars = make(map[string]string)
		}
		clean, err := cleanHostTarName(header.Name)
		if err != nil {
			return err
		}
		e.regulars[clean] = name
		return nil
	default:
		return nil
	}
}

func (e *hostTarExtractor) writeSymlink(name, target string) error {
	parent, base, err := openHostTarParent(e.root, name)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if info, err := parent.Lstat(base); err == nil {
		if info.IsDir() {
			return fmt.Errorf("copy conflict at %s: cannot overwrite directory with non-directory", name)
		}
		if info.Mode()&os.ModeSymlink == 0 && !info.Mode().IsRegular() {
			return fmt.Errorf("copy conflict at %s: refusing to overwrite special file", name)
		}
		if err := parent.Remove(base); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	// Preserve the source link text, including links that point outside the
	// copied tree. Parent traversal is safe because later archive entries are
	// opened through openHostTarDir, which never follows a symlink component.
	if err := parent.Symlink(target, base); err != nil {
		return fmt.Errorf("create symlink %s: %w", name, err)
	}
	return nil
}

func (e *hostTarExtractor) writeHardlink(name string, header *tar.Header) error {
	linkName, err := cleanHostTarName(header.Linkname)
	if err != nil {
		return fmt.Errorf("unsafe tar hard link target %q: %w", header.Linkname, err)
	}
	target, ok := e.regulars[linkName]
	if !ok {
		return fmt.Errorf("tar hard link %q targets unavailable regular file %q", header.Name, header.Linkname)
	}
	parent, base, err := openHostTarParent(e.root, name)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()
	if info, statErr := parent.Lstat(base); statErr == nil {
		if info.IsDir() || !info.Mode().IsRegular() {
			return fmt.Errorf("copy conflict at %s: refusing to overwrite non-regular file", name)
		}
	} else if !os.IsNotExist(statErr) {
		return statErr
	}
	var temporary string
	for range 100 {
		var random [12]byte
		if _, err := cryptorand.Read(random[:]); err != nil {
			return err
		}
		temporary = ".vmsh-link-" + hex.EncodeToString(random[:])
		temporaryPath := filepath.Join(filepath.Dir(name), temporary)
		if err := e.root.Link(target, temporaryPath); err == nil {
			break
		} else if !os.IsExist(err) {
			return fmt.Errorf("stage hard link %s to %s: %w", name, target, err)
		}
		temporary = ""
	}
	if temporary == "" {
		return fmt.Errorf("could not allocate temporary hard link for %s", name)
	}
	published := false
	defer func() {
		if !published {
			_ = parent.Remove(temporary)
		}
	}()
	if err := replaceHostRootFile(parent, temporary, base); err != nil {
		return fmt.Errorf("publish hard link %s to %s: %w", name, target, err)
	}
	published = true
	return nil
}

func (e *hostTarExtractor) writeRegular(name string, perm os.FileMode, mtime time.Time, header *tar.Header, tr *tar.Reader) error {
	parent, base, err := openHostTarParent(e.root, name)
	if err != nil {
		return err
	}
	defer func() { _ = parent.Close() }()

	var existing os.FileInfo
	if info, err := parent.Lstat(base); err == nil {
		switch {
		case info.IsDir():
			return fmt.Errorf("copy conflict at %s: cannot overwrite directory with non-directory", name)
		case info.Mode()&os.ModeSymlink != 0:
			return fmt.Errorf("copy conflict at %s: refusing to follow symlink", name)
		case !info.Mode().IsRegular():
			return fmt.Errorf("copy conflict at %s: refusing to overwrite special file", name)
		default:
			existing = info
		}
	} else if !os.IsNotExist(err) {
		return err
	}

	file, temporary, err := createHostTarTemp(parent, perm)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = parent.Remove(temporary)
		}
	}()
	if err := copyHostTarRegular(file, header, tr); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Chmod(perm); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if existing != nil {
		current, err := parent.Lstat(base)
		if err != nil {
			return err
		}
		if !current.Mode().IsRegular() || !os.SameFile(existing, current) {
			return fmt.Errorf("copy destination %s changed while extracting", name)
		}
	} else if _, err := parent.Lstat(base); err == nil {
		return fmt.Errorf("copy destination %s appeared while extracting", name)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := replaceHostRootFile(parent, temporary, base); err != nil {
		return err
	}
	published = true
	if !mtime.IsZero() {
		if err := parent.Chtimes(base, mtime, mtime); err != nil {
			return err
		}
	}
	return nil
}

type hostTarSparseExtent struct {
	offset int64
	length int64
}

func hostTarSparseMap(header *tar.Header) ([]hostTarSparseExtent, int64, bool, error) {
	if header == nil || header.PAXRecords == nil {
		return nil, 0, false, nil
	}
	encodedSize, present := header.PAXRecords["VMSH.sparse.size"]
	if !present {
		return nil, 0, false, nil
	}
	logicalSize, err := strconv.ParseInt(encodedSize, 10, 64)
	if err != nil || logicalSize < 0 {
		return nil, 0, false, fmt.Errorf("tar entry %q has invalid sparse size", header.Name)
	}
	count, err := strconv.Atoi(header.PAXRecords["VMSH.sparse.numblocks"])
	if err != nil || count < 0 {
		return nil, 0, false, fmt.Errorf("tar entry %q has invalid sparse extent count", header.Name)
	}
	var values []string
	if encoded := header.PAXRecords["VMSH.sparse.map"]; encoded != "" {
		values = strings.Split(encoded, ",")
	}
	if len(values) != count*2 {
		return nil, 0, false, fmt.Errorf("tar entry %q has invalid sparse map", header.Name)
	}
	extents := make([]hostTarSparseExtent, 0, count)
	var physicalSize, previousEnd int64
	for i := 0; i < len(values); i += 2 {
		offset, offsetErr := strconv.ParseInt(values[i], 10, 64)
		length, lengthErr := strconv.ParseInt(values[i+1], 10, 64)
		if offsetErr != nil || lengthErr != nil || offset < previousEnd || length < 0 || offset < 0 || offset > logicalSize-length || physicalSize > header.Size-length {
			return nil, 0, false, fmt.Errorf("tar entry %q has invalid sparse map", header.Name)
		}
		physicalSize += length
		previousEnd = offset + length
		extents = append(extents, hostTarSparseExtent{offset: offset, length: length})
	}
	if physicalSize != header.Size {
		return nil, 0, false, fmt.Errorf("tar entry %q has invalid sparse payload size", header.Name)
	}
	return extents, logicalSize, true, nil
}

func copyHostTarRegular(file *os.File, header *tar.Header, tr *tar.Reader) error {
	extents, logicalSize, sparse, err := hostTarSparseMap(header)
	if err != nil {
		return err
	}
	if !sparse {
		_, err = io.Copy(file, tr)
		return err
	}
	for _, extent := range extents {
		if _, err := file.Seek(extent.offset, io.SeekStart); err != nil {
			return err
		}
		if _, err := io.CopyN(file, tr, extent.length); err != nil {
			return err
		}
	}
	return file.Truncate(logicalSize)
}

func createHostTarTemp(root *os.Root, perm os.FileMode) (*os.File, string, error) {
	for range 100 {
		var random [12]byte
		if _, err := cryptorand.Read(random[:]); err != nil {
			return nil, "", err
		}
		name := ".vmsh-copy-" + hex.EncodeToString(random[:])
		file, err := root.OpenFile(name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm)
		if err == nil {
			return file, name, nil
		}
		if !os.IsExist(err) {
			return nil, "", err
		}
	}
	return nil, "", fmt.Errorf("could not allocate temporary copy file")
}

func openHostTarParent(root *os.Root, name string) (*os.Root, string, error) {
	if name == "." {
		return nil, "", fmt.Errorf("tar entry cannot replace extraction root")
	}
	parent, err := openHostTarDir(root, filepath.Dir(name), true, 0o755)
	if err != nil {
		return nil, "", err
	}
	base := filepath.Base(name)
	if !filepath.IsLocal(base) {
		_ = parent.Close()
		return nil, "", fmt.Errorf("unsafe tar path %q", name)
	}
	return parent, base, nil
}

// openHostTarDir walks one directory component at a time. It rejects symlink
// components before opening them and verifies that the opened handle still
// refers to the object that was inspected.
func openHostTarDir(root *os.Root, name string, create bool, perm os.FileMode) (*os.Root, error) {
	clean := filepath.Clean(name)
	if clean == "." {
		return root.OpenRoot(".")
	}
	if !filepath.IsLocal(clean) {
		return nil, fmt.Errorf("unsafe tar directory %q", name)
	}
	current, err := root.OpenRoot(".")
	if err != nil {
		return nil, err
	}
	for _, component := range strings.Split(clean, string(filepath.Separator)) {
		info, err := current.Lstat(component)
		if os.IsNotExist(err) && create {
			if err := current.Mkdir(component, perm); err != nil && !os.IsExist(err) {
				_ = current.Close()
				return nil, err
			}
			info, err = current.Lstat(component)
		}
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			_ = current.Close()
			return nil, fmt.Errorf("copy path component %s is not a real directory", component)
		}
		next, err := current.OpenRoot(component)
		if err != nil {
			_ = current.Close()
			return nil, err
		}
		opened, err := next.Stat(".")
		if err != nil || !os.SameFile(info, opened) {
			_ = next.Close()
			_ = current.Close()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("copy path component %s changed while extracting", component)
		}
		_ = current.Close()
		current = next
	}
	return current, nil
}

func (e *hostTarExtractor) restoreDirMtimes() error {
	for i := len(e.dirs) - 1; i >= 0; i-- {
		dir := e.dirs[i]
		if dir.mtime.IsZero() {
			continue
		}
		opened, err := openHostTarDir(e.root, dir.name, false, 0)
		if err != nil {
			return err
		}
		_ = opened.Close()
		if err := e.root.Chtimes(dir.name, dir.mtime, dir.mtime); err != nil {
			return err
		}
	}
	return nil
}
