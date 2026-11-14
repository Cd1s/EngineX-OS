package azimg

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"sync"
)

const (
	gen2ImgPrefix = "ENGINEX_DATA_BUNDLE"
	gen2ImgMagic  = "ENGINEX_DATA_BUNDLE_IMG_V1\n"
	gen2ImgHeader = 128
)

var (
	topRE     = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	versionRE = regexp.MustCompile(`^\d+(\.\d+){2,4}$`)
	hex64RE   = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)
	relTagRE  = regexp.MustCompile(`^v\d+(\.\d+){2,4}(-r[1-9]\d?)?$`)
)

func openGen2Image(ctx context.Context, r io.ReaderAt, size int64, limits Limits, sniff []byte) (*Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size <= gen2ImgHeader || len(sniff) < gen2ImgHeader {
		return nil, fmt.Errorf("%w: truncated ENGINEX_DATA_BUNDLE_IMG_V1 image (%d bytes)", ErrCorrupt, size)
	}
	payloadSize := size - gen2ImgHeader
	if payloadSize > limits.MaxCompressedSize {
		return nil, fmt.Errorf("%w: Gen-2 payload size %d exceeds %d limit", ErrLimitExceeded, payloadSize, limits.MaxCompressedSize)
	}
	if string(sniff[:len(gen2ImgMagic)]) != gen2ImgMagic || sniff[len(gen2ImgMagic)+64] != '\n' {
		return nil, fmt.Errorf("%w: invalid ENGINEX_DATA_BUNDLE_IMG_V1 header magic or digest terminator", ErrCorrupt)
	}

	// Validate NUL padding strictly: every byte after the first NUL byte must be NUL.
	hdrRaw := sniff[:gen2ImgHeader]
	if nul := bytes.IndexByte(hdrRaw, 0); nul >= 0 {
		for _, b := range hdrRaw[nul:] {
			if b != 0 {
				return nil, fmt.Errorf("%w: non-zero padding byte in ENGINEX_DATA_BUNDLE_IMG_V1 header", ErrCorrupt)
			}
		}
		hdrRaw = hdrRaw[:nul]
	}
	for _, b := range hdrRaw {
		if b >= 0x80 || (b < 0x20 && b != '\n') {
			return nil, fmt.Errorf("%w: non-ASCII or control byte in ENGINEX_DATA_BUNDLE_IMG_V1 header", ErrCorrupt)
		}
	}
	hdrText := string(hdrRaw)
	if !strings.HasSuffix(hdrText, "\n") {
		return nil, fmt.Errorf("%w: ENGINEX_DATA_BUNDLE_IMG_V1 header missing trailing newline", ErrCorrupt)
	}
	lines := strings.Split(strings.TrimSuffix(hdrText, "\n"), "\n")
	if len(lines) != 4 || lines[0] != "ENGINEX_DATA_BUNDLE_IMG_V1" || !hex64RE.MatchString(lines[1]) {
		return nil, fmt.Errorf("%w: invalid ENGINEX_DATA_BUNDLE_IMG_V1 header fields", ErrCorrupt)
	}
	model, ok := LookupGen2Prefix(lines[2])
	if !ok || lines[2] != model.Prefix {
		return nil, fmt.Errorf("%w: unrecognized or non-canonical model prefix %q in Gen-2 header", ErrModelMismatch, lines[2])
	}
	if !versionRE.MatchString(lines[3]) {
		return nil, fmt.Errorf("%w: invalid version %q in Gen-2 header", ErrCorrupt, lines[3])
	}

	var gzMagic [2]byte
	if _, err := io.ReadFull(io.NewSectionReader(r, gen2ImgHeader, 2), gzMagic[:]); err != nil {
		return nil, fmt.Errorf("%w: truncated Gen-2 payload: %v", ErrCorrupt, err)
	}
	if gzMagic[0] != 0x1f || gzMagic[1] != 0x8b {
		return nil, fmt.Errorf("%w: Gen-2 payload is not gzip compressed", ErrCorrupt)
	}

	wantBytes, err := hex.DecodeString(strings.ToLower(lines[1]))
	if err != nil || len(wantBytes) != 32 {
		return nil, fmt.Errorf("%w: invalid Gen-2 header SHA-256", ErrCorrupt)
	}
	var wantSHA [32]byte
	copy(wantSHA[:], wantBytes)

	if err := hashSectionSHA256(ctx, r, gen2ImgHeader, payloadSize, wantSHA); err != nil {
		return nil, fmt.Errorf("%w: Gen-2 payload: %v", ErrDigestMismatch, err)
	}

	return &Image{
		r:             r,
		size:          size,
		limits:        limits,
		format:        FormatGen2Image,
		models:        []Model{model},
		payloadOffset: gen2ImgHeader,
		payloadSize:   payloadSize,
		headerPrefix:  lines[2],
		headerVersion: lines[3],
		headerSHA256:  wantSHA,
		hasHeaderSHA:  true,
	}, nil
}

func openGen2Bundle(ctx context.Context, r io.ReaderAt, size int64, limits Limits) (*Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if size < 2 {
		return nil, fmt.Errorf("%w: truncated gzip bundle", ErrCorrupt)
	}
	if size > limits.MaxCompressedSize {
		return nil, fmt.Errorf("%w: Gen-2 bundle size %d exceeds %d limit", ErrLimitExceeded, size, limits.MaxCompressedSize)
	}
	im := &Image{
		r:             r,
		size:          size,
		limits:        limits,
		format:        FormatGen2Bundle,
		payloadOffset: 0,
		payloadSize:   size,
	}
	b, err := im.Bundle(ctx)
	if err != nil {
		return nil, err
	}
	im.models = []Model{b.model}
	return im, nil
}

func hashSectionSHA256(ctx context.Context, r io.ReaderAt, offset, size int64, want [32]byte) error {
	h := sha256.New()
	sr := io.NewSectionReader(r, offset, size)
	buf := make([]byte, 1<<20)
	var total int64
	for total < size {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := sr.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			total += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
	}
	if total != size {
		return fmt.Errorf("short read (%d of %d bytes)", total, size)
	}
	var got [32]byte
	copy(got[:], h.Sum(nil))
	if got != want {
		return fmt.Errorf("SHA-256 mismatch (got %x, want %x)", got, want)
	}
	return nil
}

type archiveMemberMeta struct {
	Path   string
	Mode   uint32
	Size   int64
	SHA256 [32]byte
	Index  int
}

// Bundle holds the validated metadata and verified archive index of a Gen-2 Data-Bundle.
type Bundle struct {
	im         *Image
	top        string
	version    string
	releaseTag string
	model      Model
	alsaDir    string
	packID     string
	hasUIPacks bool

	dirs       []string
	uiDirs     []string
	entries    []archiveMemberMeta
	uiEntries  []archiveMemberMeta
	uiTarIndex int

	cursorMu sync.Mutex
	cursor   *tarCursor
	uiCursor *uiTarCursor
}

// Top returns the bundle's single verified top-level directory name.
func (b *Bundle) Top() string { return b.top }

// Version returns the validated version string from version.txt.
func (b *Bundle) Version() string { return b.version }

// ReleaseTag returns the optional release-tag value (e.g. "v5.3.0.4-r4"), or "" if absent.
func (b *Bundle) ReleaseTag() string { return b.releaseTag }

// Model returns the validated Gen-2 hardware Model.
func (b *Bundle) Model() Model { return b.model }

// ALSADir returns the validated relative ALSA config directory from alsa-dir, or "" if absent.
func (b *Bundle) ALSADir() string { return b.alsaDir }

// PackID returns the lowercase hex SHA-256 of ui-packs.tar when present.
func (b *Bundle) PackID() string { return b.packID }

// HasUIPacks reports whether the bundle contained a validated ui-packs.tar archive.
func (b *Bundle) HasUIPacks() bool { return b.hasUIPacks }

// Dirs returns the canonical relative directory paths inside the bundle (excluding Top itself).
func (b *Bundle) Dirs() []string { return append([]string(nil), b.dirs...) }

// UIDirs returns the canonical relative directory paths inside ui-packs.tar.
func (b *Bundle) UIDirs() []string { return append([]string(nil), b.uiDirs...) }

// CompressedSize returns the size in bytes of the inner .tar.gz payload.
func (b *Bundle) CompressedSize() int64 { return b.im.payloadSize }

// OpenTarGz returns a streaming reader over the inner .tar.gz payload, verifying the
// wrapper SHA-256 immediately when CompressedSize bytes have been read (when opened
// from an ENGINEX_DATA_BUNDLE_IMG_V1 container).
func (b *Bundle) OpenTarGz(ctx context.Context) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sr := &ctxReader{ctx: ctx, r: io.NewSectionReader(b.im.r, b.im.payloadOffset, b.im.payloadSize)}
	if !b.im.hasHeaderSHA {
		return io.NopCloser(sr), nil
	}
	return &verifyingReader{
		r:    sr,
		size: b.im.payloadSize,
		want: b.im.headerSHA256,
		hash: sha256.New(),
	}, nil
}

// Bundle validates and parses the Gen-2 Data-Bundle (.tar.gz and nested ui-packs.tar).
// It is safe for concurrent use; results are cached on im after successful validation.
func (im *Image) Bundle(ctx context.Context) (*Bundle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if im.format != FormatGen2Image && im.format != FormatGen2Bundle {
		return nil, fmt.Errorf("%w: Bundle() requires a Gen-2 image or bundle", ErrUnsupported)
	}
	im.bundleMu.Lock()
	defer im.bundleMu.Unlock()
	if im.bundle != nil {
		return im.bundle, nil
	}

	b, err := parseGen2Bundle(ctx, im)
	if err != nil {
		return nil, err
	}
	im.bundle = b
	return b, nil
}

// boundedCountReader counts every uncompressed byte read (including PAX/USTAR headers and
// trailing blocks) against MaxExpandedSize and checks ctx cancellation.
type boundedCountReader struct {
	ctx   context.Context
	r     io.Reader
	n     int64
	limit int64
}

func (c *boundedCountReader) Read(p []byte) (int, error) {
	if c.ctx != nil {
		if err := c.ctx.Err(); err != nil {
			return 0, err
		}
	}
	rem := c.limit - c.n
	if rem < 0 {
		return 0, fmt.Errorf("%w: uncompressed archive stream exceeds %d byte budget", ErrLimitExceeded, c.limit)
	}
	readBuf := p
	if int64(len(readBuf)) > rem+1 {
		readBuf = readBuf[:rem+1]
	}
	n, err := c.r.Read(readBuf)
	c.n += int64(n)
	if c.n > c.limit {
		return n, fmt.Errorf("%w: uncompressed archive stream exceeds %d byte budget", ErrLimitExceeded, c.limit)
	}
	return n, err
}

type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c *ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

func parseGen2Bundle(ctx context.Context, im *Image) (*Bundle, error) {
	sr := io.NewSectionReader(im.r, im.payloadOffset, im.payloadSize)
	br := bufio.NewReaderSize(sr, 32<<10)
	gz, err := gzip.NewReader(br)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid gzip header: %v", ErrCorrupt, err)
	}
	gz.Multistream(false)
	defer gz.Close()

	// Charge every inflated byte (including PAX extended headers and tar padding) against MaxExpandedSize.
	gzOut := &boundedCountReader{ctx: ctx, r: gz, limit: im.limits.MaxExpandedSize}
	tr := tar.NewReader(gzOut)
	b := &Bundle{
		im:         im,
		uiTarIndex: -1,
	}

	seenFolded := make(map[string]bool)
	metaFiles := make(map[string][]byte)
	var uiExpanded int64
	var totalEntries int

	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, ErrLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: reading tar archive: %v", ErrCorrupt, err)
		}

		totalEntries++
		if totalEntries > im.limits.MaxArchiveEntries {
			return nil, fmt.Errorf("%w: archive entry count exceeds %d", ErrLimitExceeded, im.limits.MaxArchiveEntries)
		}

		isDir := h.Typeflag == tar.TypeDir
		isReg := h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA
		if !isDir && !isReg {
			return nil, fmt.Errorf("%w: unsupported archive entry type %q for %q", ErrUnsafePath, h.Typeflag, h.Name)
		}

		cleaned, err := CleanArchivePath(h.Name, isDir)
		if err != nil {
			return nil, err
		}
		if cleaned == "." {
			return nil, fmt.Errorf("%w: bundle root cannot be %q", ErrUnsafePath, h.Name)
		}
		folded := foldArchivePath(cleaned)
		if seenFolded[folded] {
			return nil, fmt.Errorf("%w: duplicate archive path %q", ErrCorrupt, cleaned)
		}
		seenFolded[folded] = true

		if b.top == "" {
			if slash := strings.IndexByte(cleaned, '/'); slash >= 0 {
				b.top = cleaned[:slash]
			} else {
				b.top = cleaned
			}
			if !topRE.MatchString(b.top) {
				return nil, fmt.Errorf("%w: unsafe top directory %q", ErrUnsafePath, b.top)
			}
		}

		var rel string
		if cleaned == b.top {
			if !isDir {
				return nil, fmt.Errorf("%w: top entry %q is not a directory", ErrCorrupt, cleaned)
			}
			continue
		}
		if !strings.HasPrefix(cleaned, b.top+"/") {
			return nil, fmt.Errorf("%w: member %q is outside top directory %q", ErrUnsafePath, h.Name, b.top)
		}
		rel = strings.TrimPrefix(cleaned, b.top+"/")

		if isDir {
			b.dirs = append(b.dirs, rel)
			continue
		}

		if h.Size < 0 || h.Size > im.limits.MaxMemberSize {
			return nil, fmt.Errorf("%w: member %q size %d exceeds limit %d", ErrLimitExceeded, rel, h.Size, im.limits.MaxMemberSize)
		}
		if gzOut.n+uiExpanded+h.Size > im.limits.MaxExpandedSize {
			return nil, fmt.Errorf("%w: expanded bundle size exceeds %d byte budget", ErrLimitExceeded, im.limits.MaxExpandedSize)
		}

		hash := sha256.New()
		var n int64

		switch rel {
		case "ui-packs.tar":
			b.hasUIPacks = true
			b.uiTarIndex = len(b.entries)
			lr := io.LimitReader(&ctxReader{ctx: ctx, r: tr}, h.Size+1)
			tee := io.TeeReader(lr, hash)
			uiN, newUIExpanded, uiEntriesCount, err := parseNestedUIPacksTar(ctx, tee, h.Size, im.limits, gzOut, uiExpanded, totalEntries, b)
			if err != nil {
				return nil, err
			}
			n = uiN
			uiExpanded = newUIExpanded
			totalEntries = uiEntriesCount
		case "SHA256SUMS", "version.txt", "model", "release-tag", "alsa-dir":
			maxMeta := int64(64 << 10)
			if rel == "SHA256SUMS" {
				maxMeta = im.limits.MaxManifestSize
			}
			if h.Size > maxMeta {
				return nil, fmt.Errorf("%w: metadata file %q (%d bytes) exceeds %d limit", ErrLimitExceeded, rel, h.Size, maxMeta)
			}
			var buf bytes.Buffer
			buf.Grow(int(h.Size))
			n, err = io.Copy(io.MultiWriter(hash, &buf), io.LimitReader(&ctxReader{ctx: ctx, r: tr}, h.Size+1))
			if err != nil {
				if errors.Is(err, ErrLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, err
				}
				return nil, fmt.Errorf("%w: reading %q: %v", ErrCorrupt, rel, err)
			}
			metaFiles[rel] = buf.Bytes()
		default:
			n, err = io.Copy(hash, io.LimitReader(&ctxReader{ctx: ctx, r: tr}, h.Size+1))
			if err != nil {
				if errors.Is(err, ErrLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil, err
				}
				return nil, fmt.Errorf("%w: reading %q: %v", ErrCorrupt, rel, err)
			}
		}

		if n != h.Size {
			return nil, fmt.Errorf("%w: member %q size mismatch (got %d, header says %d)", ErrCorrupt, rel, n, h.Size)
		}
		if gzOut.n+uiExpanded > im.limits.MaxExpandedSize {
			return nil, fmt.Errorf("%w: total expanded size %d exceeds %d limit", ErrLimitExceeded, gzOut.n+uiExpanded, im.limits.MaxExpandedSize)
		}

		var sum [32]byte
		copy(sum[:], hash.Sum(nil))
		if rel == "ui-packs.tar" {
			b.packID = hex.EncodeToString(sum[:])
		}
		b.entries = append(b.entries, archiveMemberMeta{
			Path:   rel,
			Mode:   uint32(h.Mode) & 0o777,
			Size:   n,
			SHA256: sum,
			Index:  len(b.entries),
		})
	}

	// Drain tar EOF blocks through gzOut so gzip verifies CRC32/ISIZE within MaxExpandedSize.
	if err := drainBoundedGzipEOF(gzOut, im.limits.MaxExpandedSize-gzOut.n-uiExpanded); err != nil {
		return nil, err
	}
	if _, err := br.ReadByte(); err != io.EOF {
		return nil, fmt.Errorf("%w: trailing or concatenated data after gzip stream", ErrCorrupt)
	}

	if err := verifyBundleManifestAndMeta(b, im, metaFiles); err != nil {
		return nil, err
	}
	return b, nil
}

func drainBoundedGzipEOF(r io.Reader, remainingBudget int64) error {
	if remainingBudget < 0 {
		remainingBudget = 0
	}
	n, err := io.Copy(io.Discard, io.LimitReader(r, remainingBudget+1))
	if err != nil {
		if errors.Is(err, ErrLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return err
		}
		return fmt.Errorf("%w: gzip trailer check failed: %v", ErrCorrupt, err)
	}
	if n > remainingBudget {
		return fmt.Errorf("%w: trailing gzip data exceeds expanded budget", ErrLimitExceeded)
	}
	return nil
}

func parseNestedUIPacksTar(
	ctx context.Context,
	r io.Reader,
	tarSize int64,
	limits Limits,
	gzOut *boundedCountReader,
	uiExpandedSoFar int64,
	entriesSoFar int,
	b *Bundle,
) (tarRead int64, newUIExpanded int64, newEntries int, err error) {
	cr := &countingSHA1Reader{r: r}
	tr := tar.NewReader(cr)
	seenFolded := make(map[string]bool)
	hasPacks := false
	uiExpanded := uiExpandedSoFar
	entries := entriesSoFar

	for {
		if err := ctx.Err(); err != nil {
			return cr.n, uiExpanded, entries, err
		}
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if errors.Is(err, ErrLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return cr.n, uiExpanded, entries, err
			}
			return cr.n, uiExpanded, entries, fmt.Errorf("%w: reading nested ui-packs.tar: %v", ErrCorrupt, err)
		}
		entries++
		if entries > limits.MaxArchiveEntries {
			return cr.n, uiExpanded, entries, fmt.Errorf("%w: archive entry count exceeds %d (ui-packs.tar)", ErrLimitExceeded, limits.MaxArchiveEntries)
		}

		isDir := h.Typeflag == tar.TypeDir
		isReg := h.Typeflag == tar.TypeReg || h.Typeflag == tar.TypeRegA
		if !isDir && !isReg {
			return cr.n, uiExpanded, entries, fmt.Errorf("%w: unsupported nested ui-packs.tar entry type %q for %q", ErrUnsafePath, h.Typeflag, h.Name)
		}

		cleaned, err := CleanArchivePath(h.Name, isDir)
		if err != nil {
			return cr.n, uiExpanded, entries, err
		}
		if cleaned == "." {
			continue
		}
		folded := foldArchivePath(cleaned)
		if seenFolded[folded] {
			return cr.n, uiExpanded, entries, fmt.Errorf("%w: duplicate nested ui-packs.tar path %q", ErrCorrupt, cleaned)
		}
		seenFolded[folded] = true

		if cleaned == "packs" || strings.HasPrefix(cleaned, "packs/") {
			hasPacks = true
		}

		if isDir {
			b.uiDirs = append(b.uiDirs, cleaned)
			continue
		}

		if h.Size < 0 || h.Size > limits.MaxMemberSize {
			return cr.n, uiExpanded, entries, fmt.Errorf("%w: nested UI member %q size %d exceeds limit", ErrLimitExceeded, cleaned, h.Size)
		}
		if gzOut.n+uiExpanded+h.Size > limits.MaxExpandedSize {
			return cr.n, uiExpanded, entries, fmt.Errorf("%w: combined bundle + UI expanded size exceeds %d budget", ErrLimitExceeded, limits.MaxExpandedSize)
		}
		hash := sha256.New()
		n, err := io.Copy(hash, io.LimitReader(&ctxReader{ctx: ctx, r: tr}, h.Size+1))
		if err != nil {
			if errors.Is(err, ErrLimitExceeded) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return cr.n, uiExpanded, entries, err
			}
			return cr.n, uiExpanded, entries, fmt.Errorf("%w: reading nested UI member %q: %v", ErrCorrupt, cleaned, err)
		}
		if n != h.Size {
			return cr.n, uiExpanded, entries, fmt.Errorf("%w: nested UI member %q size mismatch", ErrCorrupt, cleaned)
		}
		uiExpanded += n

		var sum [32]byte
		copy(sum[:], hash.Sum(nil))
		b.uiEntries = append(b.uiEntries, archiveMemberMeta{
			Path:   cleaned,
			Mode:   uint32(h.Mode) & 0o777,
			Size:   n,
			SHA256: sum,
			Index:  len(b.uiEntries),
		})
	}

	if !hasPacks {
		return cr.n, uiExpanded, entries, fmt.Errorf("%w: ui-packs.tar has no packs/ directory", ErrCorrupt)
	}
	// Drain trailing tar zero-blocks so the outer SHA-256 covers the complete ui-packs.tar.
	rem := tarSize - cr.n
	if rem < 0 {
		return cr.n, uiExpanded, entries, fmt.Errorf("%w: ui-packs.tar exceeded declared tar size", ErrCorrupt)
	}
	if _, err := io.Copy(io.Discard, io.LimitReader(cr, rem+1)); err != nil {
		return cr.n, uiExpanded, entries, fmt.Errorf("%w: draining ui-packs.tar trailer: %v", ErrCorrupt, err)
	}
	if cr.n != tarSize {
		return cr.n, uiExpanded, entries, fmt.Errorf("%w: ui-packs.tar read %d of %d bytes", ErrCorrupt, cr.n, tarSize)
	}
	return cr.n, uiExpanded, entries, nil
}

func trimTrailingNewline(b []byte) string {
	s := string(b)
	s = strings.TrimSuffix(s, "\n")
	s = strings.TrimSuffix(s, "\r")
	return s
}

func verifyBundleManifestAndMeta(b *Bundle, im *Image, meta map[string][]byte) error {
	rawSums, ok := meta["SHA256SUMS"]
	if !ok || len(bytes.TrimSpace(rawSums)) == 0 {
		return fmt.Errorf("%w: SHA256SUMS missing or empty in bundle", ErrCorrupt)
	}

	byRel := make(map[string]archiveMemberMeta, len(b.entries))
	for _, e := range b.entries {
		byRel[e.Path] = e
	}

	listed := make(map[string]bool)
	listedFolded := make(map[string]bool)
	for _, line := range strings.Split(string(rawSums), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if line == "" {
			continue
		}
		if len(line) < 66 || !hex64RE.MatchString(line[:64]) || (line[64] != ' ' && line[64] != '\t') {
			return fmt.Errorf("%w: malformed SHA256SUMS line %q", ErrCorrupt, line)
		}
		wantHex := strings.ToLower(line[:64])
		relRaw := strings.TrimLeft(line[64:], " \t")
		rel, err := CleanArchivePath(relRaw, false)
		if err != nil {
			return fmt.Errorf("%w: unsafe path in SHA256SUMS: %v", ErrUnsafePath, err)
		}
		folded := foldArchivePath(rel)
		if listed[rel] || listedFolded[folded] {
			return fmt.Errorf("%w: duplicate entry %q in SHA256SUMS", ErrCorrupt, rel)
		}
		listed[rel] = true
		listedFolded[folded] = true

		ent, exists := byRel[rel]
		if !exists {
			return fmt.Errorf("%w: SHA256SUMS references missing member %q", ErrCorrupt, rel)
		}
		gotHex := hex.EncodeToString(ent.SHA256[:])
		if gotHex != wantHex {
			return fmt.Errorf("%w: SHA256SUMS mismatch for %q (got %s, manifest says %s)", ErrDigestMismatch, rel, gotHex, wantHex)
		}
	}

	// Require a consistent manifest: every regular member except SHA256SUMS must be listed.
	for _, e := range b.entries {
		if e.Path == "SHA256SUMS" {
			continue
		}
		if !listed[e.Path] {
			return fmt.Errorf("%w: unlisted regular bundle member %q missing from SHA256SUMS", ErrCorrupt, e.Path)
		}
	}

	verBytes, hasVer := meta["version.txt"]
	if !hasVer {
		return fmt.Errorf("%w: version.txt missing from bundle", ErrCorrupt)
	}
	b.version = trimTrailingNewline(verBytes)
	if !versionRE.MatchString(b.version) {
		return fmt.Errorf("%w: invalid version.txt value %q", ErrCorrupt, b.version)
	}
	if im.headerVersion != "" && b.version != im.headerVersion {
		return fmt.Errorf("%w: version.txt (%q) does not match header version (%q)", ErrCorrupt, b.version, im.headerVersion)
	}

	var prefix string
	if modelBytes, ok := meta["model"]; ok {
		prefix = trimTrailingNewline(modelBytes)
		m, ok := LookupGen2Prefix(prefix)
		if !ok || prefix != m.Prefix {
			return fmt.Errorf("%w: invalid bundle model %q", ErrModelMismatch, prefix)
		}
		if im.headerPrefix != "" && prefix != im.headerPrefix {
			return fmt.Errorf("%w: bundle model (%q) does not match header (%q)", ErrModelMismatch, prefix, im.headerPrefix)
		}
		b.model = m
	} else if im.headerPrefix != "" {
		m, _ := LookupGen2Prefix(im.headerPrefix)
		b.model = m
	} else {
		topPrefix := strings.SplitN(b.top, "-", 2)[0]
		m, ok := LookupGen2Prefix(topPrefix)
		if !ok {
			return fmt.Errorf("%w: cannot determine Gen-2 model from bundle %q", ErrModelMismatch, b.top)
		}
		b.model = m
	}

	if !strings.HasPrefix(b.top, b.model.Prefix+"-") {
		return fmt.Errorf("%w: bundle top directory %q does not match model %q", ErrModelMismatch, b.top, b.model.Prefix)
	}

	if tagBytes, ok := meta["release-tag"]; ok {
		tag := trimTrailingNewline(tagBytes)
		if !relTagRE.MatchString(tag) || (tag != "v"+b.version && !strings.HasPrefix(tag, "v"+b.version+"-r")) {
			return fmt.Errorf("%w: invalid release-tag %q for version %q", ErrCorrupt, tag, b.version)
		}
		b.releaseTag = tag
	}

	if alsaBytes, ok := meta["alsa-dir"]; ok {
		alsaVal := trimTrailingNewline(alsaBytes)
		wantALSA := "system/var/overlay/lib/alsa/conf.d"
		if b.model.AZ04 {
			wantALSA = "system/var-lib/alsa/conf.d"
		}
		if alsaVal != wantALSA {
			return fmt.Errorf("%w: invalid alsa-dir %q for %s (want %q)", ErrCorrupt, alsaVal, b.model.Prefix, wantALSA)
		}
		b.alsaDir = alsaVal
	}
	return nil
}

// Walk invokes fn for every verified regular file in the outer bundle (in archive order).
// Entry.Open works both during fn and after Walk returns, without extracting to host disk.
func (b *Bundle) Walk(ctx context.Context, fn func(Entry) error) error {
	for _, meta := range b.entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := meta
		ent := Entry{
			Path:   m.Path,
			Mode:   m.Mode,
			Size:   m.Size,
			SHA256: m.SHA256,
			Open: func() (io.ReadCloser, error) {
				return b.openOuterEntry(m)
			},
		}
		if err := fn(ent); err != nil {
			return err
		}
	}
	return nil
}

// WalkUI invokes fn for every verified regular file inside the nested ui-packs.tar archive.
// Entry.Open works both during fn and after WalkUI returns, without extracting to host disk.
func (b *Bundle) WalkUI(ctx context.Context, fn func(Entry) error) error {
	for _, meta := range b.uiEntries {
		if err := ctx.Err(); err != nil {
			return err
		}
		m := meta
		ent := Entry{
			Path:   m.Path,
			Mode:   m.Mode,
			Size:   m.Size,
			SHA256: m.SHA256,
			Open: func() (io.ReadCloser, error) {
				return b.openUIEntry(m)
			},
		}
		if err := fn(ent); err != nil {
			return err
		}
	}
	return nil
}

type tarCursor struct {
	gz      *gzip.Reader
	tr      *tar.Reader
	nextIdx int
	inUse   bool
}

type uiTarCursor struct {
	parentRC io.ReadCloser
	tr       *tar.Reader
	nextIdx  int
	inUse    bool
}

func (c *tarCursor) close() {
	if c != nil && c.gz != nil {
		_ = c.gz.Close()
	}
}

func (c *uiTarCursor) close() {
	if c != nil && c.parentRC != nil {
		_ = c.parentRC.Close()
	}
}

func newTarCursor(im *Image) (*tarCursor, error) {
	sr := io.NewSectionReader(im.r, im.payloadOffset, im.payloadSize)
	gz, err := gzip.NewReader(sr)
	if err != nil {
		return nil, err
	}
	gz.Multistream(false)
	return &tarCursor{
		gz: gz,
		tr: tar.NewReader(gz),
	}, nil
}

func (c *tarCursor) advanceTo(target int) error {
	for {
		h, err := c.tr.Next()
		if err != nil {
			return err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			continue
		}
		idx := c.nextIdx
		c.nextIdx++
		if idx == target {
			return nil
		}
		if idx > target {
			return fmt.Errorf("%w: tar cursor overshot target %d", ErrCorrupt, target)
		}
	}
}

func (b *Bundle) openOuterEntry(m archiveMemberMeta) (io.ReadCloser, error) {
	b.cursorMu.Lock()
	var c *tarCursor
	shared := false
	if b.cursor != nil && !b.cursor.inUse && b.cursor.nextIdx <= m.Index {
		c = b.cursor
		c.inUse = true
		shared = true
	}
	b.cursorMu.Unlock()

	if c == nil {
		var err error
		c, err = newTarCursor(b.im)
		if err != nil {
			return nil, err
		}
		c.inUse = true
		b.cursorMu.Lock()
		if b.cursor == nil {
			b.cursor = c
			shared = true
		} else if !b.cursor.inUse {
			b.cursor.close()
			b.cursor = c
			shared = true
		}
		b.cursorMu.Unlock()
	}

	if err := c.advanceTo(m.Index); err != nil {
		b.releaseOuterCursor(c, shared, true)
		return nil, err
	}

	return &verifyingReader{
		r:    io.LimitReader(c.tr, m.Size),
		size: m.Size,
		want: m.SHA256,
		hash: sha256.New(),
		onClose: func() error {
			b.releaseOuterCursor(c, shared, false)
			return nil
		},
	}, nil
}

func (b *Bundle) releaseOuterCursor(c *tarCursor, shared, failed bool) {
	if !shared || failed {
		c.close()
	}
	b.cursorMu.Lock()
	defer b.cursorMu.Unlock()
	if shared {
		if failed && b.cursor == c {
			b.cursor = nil
		} else {
			c.inUse = false
		}
	}
}

func (b *Bundle) openUIEntry(m archiveMemberMeta) (io.ReadCloser, error) {
	if !b.hasUIPacks || b.uiTarIndex < 0 {
		return nil, fmt.Errorf("%w: bundle has no ui-packs.tar", ErrCorrupt)
	}

	b.cursorMu.Lock()
	var uc *uiTarCursor
	shared := false
	if b.uiCursor != nil && !b.uiCursor.inUse && b.uiCursor.nextIdx <= m.Index {
		uc = b.uiCursor
		uc.inUse = true
		shared = true
	}
	b.cursorMu.Unlock()

	if uc == nil {
		uiMeta := b.entries[b.uiTarIndex]
		c, err := newTarCursor(b.im)
		if err != nil {
			return nil, err
		}
		if err := c.advanceTo(uiMeta.Index); err != nil {
			c.close()
			return nil, err
		}
		uc = &uiTarCursor{
			parentRC: &cursorCloser{c: c},
			tr:       tar.NewReader(io.LimitReader(c.tr, uiMeta.Size)),
			inUse:    true,
		}
		b.cursorMu.Lock()
		if b.uiCursor == nil {
			b.uiCursor = uc
			shared = true
		} else if !b.uiCursor.inUse {
			b.uiCursor.close()
			b.uiCursor = uc
			shared = true
		}
		b.cursorMu.Unlock()
	}

	for {
		h, err := uc.tr.Next()
		if err != nil {
			b.releaseUICursor(uc, shared, true)
			return nil, err
		}
		if h.Typeflag != tar.TypeReg && h.Typeflag != tar.TypeRegA {
			continue
		}
		idx := uc.nextIdx
		uc.nextIdx++
		if idx == m.Index {
			break
		}
		if idx > m.Index {
			b.releaseUICursor(uc, shared, true)
			return nil, fmt.Errorf("%w: UI tar cursor overshot target %d", ErrCorrupt, m.Index)
		}
	}

	return &verifyingReader{
		r:    io.LimitReader(uc.tr, m.Size),
		size: m.Size,
		want: m.SHA256,
		hash: sha256.New(),
		onClose: func() error {
			b.releaseUICursor(uc, shared, false)
			return nil
		},
	}, nil
}

func (b *Bundle) releaseUICursor(uc *uiTarCursor, shared, failed bool) {
	if !shared || failed {
		uc.close()
	}
	b.cursorMu.Lock()
	defer b.cursorMu.Unlock()
	if shared {
		if failed && b.uiCursor == uc {
			b.uiCursor = nil
		} else {
			uc.inUse = false
		}
	}
}

type cursorCloser struct{ c *tarCursor }

func (cc *cursorCloser) Read(p []byte) (int, error) { return cc.c.tr.Read(p) }
func (cc *cursorCloser) Close() error               { cc.c.close(); return nil }

type verifyingReader struct {
	r         io.Reader
	size      int64
	readN     int64
	want      [32]byte
	hash      hashWriter
	done      bool
	verifyErr error
	closed    bool
	onClose   func() error
}

type hashWriter interface {
	io.Writer
	Sum([]byte) []byte
}

func (v *verifyingReader) Read(p []byte) (int, error) {
	if v.verifyErr != nil {
		return 0, v.verifyErr
	}
	n, err := v.r.Read(p)
	if n > 0 {
		v.hash.Write(p[:n])
		v.readN += int64(n)
	}
	// Verify immediately as soon as size bytes have been read (so exact-size ReadFull /
	// CopyN never return tampered bytes with nil err), as well as on early EOF.
	if (v.readN == v.size || err == io.EOF) && !v.done {
		v.done = true
		if v.readN != v.size {
			clear(p[:n])
			v.verifyErr = fmt.Errorf("%w: truncated stream (%d of %d bytes)", ErrCorrupt, v.readN, v.size)
			return 0, v.verifyErr
		}
		var got [32]byte
		copy(got[:], v.hash.Sum(nil))
		if got != v.want {
			// Return 0 bytes so io.ReadAtLeast / io.ReadFull (which clears err when n >= min)
			// never drops ErrDigestMismatch or exposes tampered data.
			clear(p[:n])
			v.verifyErr = fmt.Errorf("%w: entry SHA-256 mismatch during read", ErrDigestMismatch)
			return 0, v.verifyErr
		}
	}
	return n, err
}

func (v *verifyingReader) Close() error {
	if v.closed {
		return nil
	}
	v.closed = true
	if v.onClose != nil {
		return v.onClose()
	}
	return nil
}
