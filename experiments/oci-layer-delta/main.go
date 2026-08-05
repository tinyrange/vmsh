package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"sort"
	"time"
)

const (
	fileCount = 128
	fileSize  = 1 << 20
)

type fileSpec struct {
	name    string
	seed    uint64
	size    int
	patchAt int
	patch   []byte
}

type chunk struct {
	rawHash    [32]byte
	compressed []byte
}

type encodedLayer struct {
	chunks []chunk
	blob   []byte
}

type chunker struct {
	name  string
	split func([]byte) [][]byte
}

type result struct {
	strategy       string
	variant        string
	targetBytes    int
	wholeBytes     int
	patchBytes     int
	reusedBytes    int
	chunks         int
	missingChunks  int
	missingRanges  int
	manifestBytes  int
	compressionPct float64
	patchPct       float64
}

func main() {
	if len(os.Args) == 3 {
		must(runRealLayers(os.Args[1], os.Args[2]))
		return
	}
	baseFiles := makeFiles()
	variants := map[string][]fileSpec{
		"64KiB in-place edit":  mutateInPlace(baseFiles),
		"2MiB early insertion": insertEarly(baseFiles),
		"package-like update":  packageUpdate(baseFiles),
		"all mtimes changed":   append([]fileSpec(nil), baseFiles...),
	}

	baseTar := makeTar(baseFiles, false)
	chunkers := []chunker{
		{name: "fixed-256KiB", split: func(b []byte) [][]byte { return fixedChunks(b, 256<<10) }},
		{name: "fixed-1MiB", split: func(b []byte) [][]byte { return fixedChunks(b, 1<<20) }},
		{name: "fixed-4MiB", split: func(b []byte) [][]byte { return fixedChunks(b, 4<<20) }},
		{name: "cdc-256KiB", split: func(b []byte) [][]byte { return contentDefinedChunks(b, 256<<10) }},
		{name: "cdc-1MiB", split: func(b []byte) [][]byte { return contentDefinedChunks(b, 1<<20) }},
		{name: "cdc-4MiB", split: func(b []byte) [][]byte { return contentDefinedChunks(b, 4<<20) }},
	}
	baseEncoded := make(map[string]encodedLayer, len(chunkers))
	for _, strategy := range chunkers {
		baseEncoded[strategy.name] = encode(strategy.split(baseTar))
	}

	var results []result
	for variant, files := range variants {
		mtimeChurn := variant == "all mtimes changed"
		targetTar := makeTar(files, mtimeChurn)
		wholeTarget := gzipBytes(targetTar)
		results = append(results, result{
			strategy:       "ordinary-OCI-gzip",
			variant:        variant,
			targetBytes:    len(wholeTarget),
			wholeBytes:     len(wholeTarget),
			patchBytes:     len(wholeTarget),
			chunks:         1,
			missingChunks:  1,
			missingRanges:  1,
			compressionPct: percent(len(wholeTarget), len(targetTar)),
			patchPct:       100,
		})
		for _, strategy := range chunkers {
			target := encode(strategy.split(targetTar))
			res := compare(baseEncoded[strategy.name], target)
			res.strategy = strategy.name
			res.variant = variant
			res.wholeBytes = len(wholeTarget)
			res.compressionPct = percent(len(target.blob), len(targetTar))
			results = append(results, res)
		}
	}

	sort.Slice(results, func(i, j int) bool {
		if results[i].variant == results[j].variant {
			return results[i].strategy < results[j].strategy
		}
		return results[i].variant < results[j].variant
	})
	fmt.Printf("synthetic layer: %d files x %d MiB = %.1f MiB tar\n", fileCount, fileSize>>20, float64(len(baseTar))/(1<<20))
	fmt.Printf("%-22s %-18s %10s %9s %10s %8s %8s %8s %8s %9s\n", "mutation", "strategy", "blob MiB", "vs gzip", "patch MiB", "patch %", "chunks", "missing", "ranges", "map KiB")
	for _, r := range results {
		fmt.Printf("%-22s %-18s %10.2f %8.2f%% %10.2f %7.2f%% %8d %8d %8d %9.1f\n",
			r.variant, r.strategy,
			float64(r.targetBytes)/(1<<20), percent(r.targetBytes-r.wholeBytes, r.wholeBytes),
			float64(r.patchBytes)/(1<<20), r.patchPct,
			r.chunks, r.missingChunks, r.missingRanges, float64(r.manifestBytes)/1024)
	}
	fmt.Println("\nchunk-map estimate uses 48 bytes per chunk; reconstruction and gzip/tar validity are verified in-process")
}

type reusableChunk struct {
	compressedHash [32]byte
	size           int
}

func runRealLayers(oldPath, newPath string) error {
	oldInfo, err := os.Stat(oldPath)
	if err != nil {
		return err
	}
	newInfo, err := os.Stat(newPath)
	if err != nil {
		return err
	}
	fmt.Printf("real layers: old %.1f MiB compressed, new %.1f MiB compressed\n", float64(oldInfo.Size())/(1<<20), float64(newInfo.Size())/(1<<20))
	fmt.Printf("%-18s %12s %12s %9s %9s %9s %10s\n", "strategy", "encoded MiB", "patch MiB", "patch %", "chunks", "missing", "map KiB")
	for _, strategy := range []struct {
		name    string
		average int
		cdc     bool
		files   bool
	}{
		{name: "fixed-256KiB", average: 256 << 10},
		{name: "cdc-256KiB", average: 256 << 10, cdc: true},
		{name: "file-fixed-256KiB", average: 256 << 10, files: true},
		{name: "file-cdc-256KiB", average: 256 << 10, cdc: true, files: true},
		{name: "fixed-1MiB", average: 1 << 20},
		{name: "cdc-1MiB", average: 1 << 20, cdc: true},
	} {
		available := make(map[[32]byte]reusableChunk)
		scan := scanCompressedLayer
		if strategy.files {
			scan = scanTarLayer
		}
		_, _, err := scan(oldPath, strategy.average, strategy.cdc, func(raw []byte) {
			compressed := gzipBytes(raw)
			available[sha256.Sum256(raw)] = reusableChunk{compressedHash: sha256.Sum256(compressed), size: len(compressed)}
		})
		if err != nil {
			return fmt.Errorf("scan old layer with %s: %w", strategy.name, err)
		}
		var encodedBytes, patchBytes, chunks, missing int
		_, _, err = scan(newPath, strategy.average, strategy.cdc, func(raw []byte) {
			compressed := gzipBytes(raw)
			encodedBytes += len(compressed)
			chunks++
			old, ok := available[sha256.Sum256(raw)]
			if !ok || old.size != len(compressed) || old.compressedHash != sha256.Sum256(compressed) {
				patchBytes += len(compressed)
				missing++
			}
		})
		if err != nil {
			return fmt.Errorf("scan new layer with %s: %w", strategy.name, err)
		}
		fmt.Printf("%-18s %12.2f %12.2f %8.2f%% %9d %9d %10.1f\n",
			strategy.name,
			float64(encodedBytes)/(1<<20), float64(patchBytes)/(1<<20), percent(patchBytes, encodedBytes),
			chunks, missing, float64(chunks*48)/1024)
	}
	return nil
}

func scanTarLayer(path string, average int, cdc bool, visit func([]byte)) (int64, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	zr, err := gzip.NewReader(file)
	if err != nil {
		return 0, 0, err
	}
	defer zr.Close()
	tr := tar.NewReader(zr)
	var rawBytes int64
	chunks := 0
	for {
		hdr, nextErr := tr.Next()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return rawBytes, chunks, nextErr
		}
		metadata := []byte(fmt.Sprintf("metadata\x00%s\x00%d\x00%d\x00%d\x00%d\x00%d\x00%d\x00%s\x00%d",
			hdr.Name, hdr.Typeflag, hdr.Mode, hdr.Uid, hdr.Gid, hdr.Size, hdr.ModTime.UnixNano(), hdr.Linkname, hdr.Devmajor<<32|hdr.Devminor))
		visit(metadata)
		rawBytes += int64(len(metadata))
		chunks++
		entryBytes, entryChunks, scanErr := scanReader(tr, average, cdc, visit)
		rawBytes += entryBytes
		chunks += entryChunks
		if scanErr != nil {
			return rawBytes, chunks, scanErr
		}
	}
	return rawBytes, chunks, nil
}

func scanCompressedLayer(path string, average int, cdc bool, visit func([]byte)) (int64, int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	defer file.Close()
	zr, err := gzip.NewReader(file)
	if err != nil {
		return 0, 0, err
	}
	defer zr.Close()
	return scanReader(zr, average, cdc, visit)
}

func scanReader(reader io.Reader, average int, cdc bool, visit func([]byte)) (int64, int, error) {
	minimum, maximum := average/4, average*4
	mask := uint64(average - 1)
	gear := gearTable()
	current := make([]byte, 0, maximum)
	buffer := make([]byte, 128<<10)
	var rolling uint64
	var rawBytes int64
	chunks := 0
	emit := func() {
		if len(current) == 0 {
			return
		}
		visit(current)
		rawBytes += int64(len(current))
		chunks++
		current = make([]byte, 0, maximum)
		rolling = 0
	}
	for {
		n, readErr := reader.Read(buffer)
		for _, value := range buffer[:n] {
			current = append(current, value)
			boundary := len(current) >= average
			if cdc {
				if len(current) >= minimum {
					rolling = (rolling << 1) + gear[value]
				}
				boundary = len(current) >= maximum || (len(current) >= minimum && rolling&mask == 0)
			}
			if boundary {
				emit()
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return rawBytes, chunks, readErr
		}
	}
	emit()
	return rawBytes, chunks, nil
}

func makeFiles() []fileSpec {
	files := make([]fileSpec, 0, fileCount)
	for i := 0; i < fileCount; i++ {
		files = append(files, fileSpec{name: fmt.Sprintf("usr/lib/packages/pkg-%04d.bin", i), seed: uint64(i + 1), size: fileSize, patchAt: -1})
	}
	return files
}

func mutateInPlace(in []fileSpec) []fileSpec {
	out := append([]fileSpec(nil), in...)
	out[63].patchAt = 400 << 10
	out[63].patch = bytes.Repeat([]byte("changed-package-data-"), (64<<10)/len("changed-package-data-")+1)[:64<<10]
	return out
}

func insertEarly(in []fileSpec) []fileSpec {
	out := append([]fileSpec(nil), in...)
	out = append(out, fileSpec{name: "usr/lib/packages/pkg-0000-added.bin", seed: 99991, size: 2 << 20, patchAt: -1})
	sort.Slice(out, func(i, j int) bool { return out[i].name < out[j].name })
	return out
}

func packageUpdate(in []fileSpec) []fileSpec {
	out := append([]fileSpec(nil), in...)
	for i := 5; i < len(out); i += 13 {
		out[i].patchAt = 128 << 10
		out[i].patch = bytes.Repeat([]byte(fmt.Sprintf("update-%04d-", i)), (128<<10)/12+1)[:128<<10]
	}
	filtered := out[:0]
	for i, f := range out {
		if i == 31 || i == 97 {
			continue
		}
		filtered = append(filtered, f)
	}
	for i := 0; i < 3; i++ {
		filtered = append(filtered, fileSpec{name: fmt.Sprintf("usr/lib/packages/new-%04d.bin", i), seed: uint64(80000 + i), size: fileSize, patchAt: -1})
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].name < filtered[j].name })
	return filtered
}

func makeTar(files []fileSpec, churnMtime bool) []byte {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for i, spec := range files {
		mtime := time.Unix(1_700_000_000, 0)
		if churnMtime {
			mtime = mtime.Add(time.Duration(i+1) * time.Second)
		}
		hdr := &tar.Header{Name: spec.name, Mode: 0o644, Size: int64(spec.size), ModTime: mtime, Uid: 0, Gid: 0, Format: tar.FormatPAX}
		must(tw.WriteHeader(hdr))
		data := fileData(spec)
		_, err := tw.Write(data)
		must(err)
	}
	must(tw.Close())
	return out.Bytes()
}

func fileData(spec fileSpec) []byte {
	out := make([]byte, spec.size)
	state := spec.seed*0x9e3779b97f4a7c15 + 1
	block := make([]byte, 4096)
	for off := 0; off < len(out); {
		for i := range block {
			state ^= state << 13
			state ^= state >> 7
			state ^= state << 17
			block[i] = byte(state)
		}
		for repeat := 0; repeat < 4 && off < len(out); repeat++ {
			off += copy(out[off:], block)
		}
	}
	if spec.patchAt >= 0 {
		copy(out[spec.patchAt:], spec.patch)
	}
	return out
}

func fixedChunks(data []byte, size int) [][]byte {
	var out [][]byte
	for len(data) > 0 {
		n := min(size, len(data))
		out = append(out, data[:n])
		data = data[n:]
	}
	return out
}

func contentDefinedChunks(data []byte, average int) [][]byte {
	minimum, maximum := average/4, average*4
	mask := uint64(average - 1)
	gear := gearTable()
	var out [][]byte
	for start := 0; start < len(data); {
		end := min(start+minimum, len(data))
		limit := min(start+maximum, len(data))
		var hash uint64
		for end < limit {
			hash = (hash << 1) + gear[data[end]]
			end++
			if hash&mask == 0 {
				break
			}
		}
		out = append(out, data[start:end])
		start = end
	}
	return out
}

func gearTable() [256]uint64 {
	var table [256]uint64
	x := uint64(0x6a09e667f3bcc909)
	for i := range table {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		table[i] = x
	}
	return table
}

func encode(rawChunks [][]byte) encodedLayer {
	var layer encodedLayer
	for _, raw := range rawChunks {
		compressed := gzipBytes(raw)
		layer.chunks = append(layer.chunks, chunk{rawHash: sha256.Sum256(raw), compressed: compressed})
		layer.blob = append(layer.blob, compressed...)
	}
	decoded := gunzipBytes(layer.blob)
	var raw bytes.Buffer
	for _, piece := range rawChunks {
		raw.Write(piece)
	}
	if !bytes.Equal(decoded, raw.Bytes()) {
		panic("concatenated gzip members did not reproduce the tar stream")
	}
	tr := tar.NewReader(bytes.NewReader(decoded))
	for {
		_, err := tr.Next()
		if err == io.EOF {
			break
		}
		must(err)
	}
	return layer
}

func compare(base, target encodedLayer) result {
	available := make(map[[32]byte][]byte, len(base.chunks))
	for _, c := range base.chunks {
		available[c.rawHash] = c.compressed
	}
	var rebuilt []byte
	missing, ranges, patchBytes := 0, 0, 0
	previousMissing := false
	for _, c := range target.chunks {
		if compressed, ok := available[c.rawHash]; ok && bytes.Equal(compressed, c.compressed) {
			rebuilt = append(rebuilt, compressed...)
			previousMissing = false
			continue
		}
		rebuilt = append(rebuilt, c.compressed...)
		patchBytes += len(c.compressed)
		missing++
		if !previousMissing {
			ranges++
		}
		previousMissing = true
	}
	if !bytes.Equal(rebuilt, target.blob) {
		panic("delta reconstruction did not reproduce target layer")
	}
	return result{
		targetBytes:   len(target.blob),
		patchBytes:    patchBytes,
		reusedBytes:   len(target.blob) - patchBytes,
		chunks:        len(target.chunks),
		missingChunks: missing,
		missingRanges: ranges,
		manifestBytes: len(target.chunks) * 48,
		patchPct:      percent(patchBytes, len(target.blob)),
	}
}

func gzipBytes(data []byte) []byte {
	var out bytes.Buffer
	zw, err := gzip.NewWriterLevel(&out, gzip.DefaultCompression)
	must(err)
	zw.Header.ModTime = time.Unix(0, 0)
	zw.Header.OS = 255
	_, err = zw.Write(data)
	must(err)
	must(zw.Close())
	return out.Bytes()
}

func gunzipBytes(data []byte) []byte {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	must(err)
	decoded, err := io.ReadAll(zr)
	must(err)
	must(zr.Close())
	return decoded
}

func percent(part, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(part) * 100 / float64(total)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
