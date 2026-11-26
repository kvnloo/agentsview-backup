package parser

import (
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"

	"go.kenn.io/agentsview/internal/assets"
)

// maxCodeBuddyImageBytes bounds one attached image read into the asset
// store, matching the largest image the CodeBuddy clients accept.
const maxCodeBuddyImageBytes = 64 << 20

// codebuddyBlobAssets resolves CBC image references against the CodeBuddy
// home's content-addressed blob store, and a converted conversation's
// codebuddy-asset:// images against its 4.x assets directory. Only
// <home>/blobs/<prefix>/<sha>.<ext> and plain file names inside that assets
// directory are read; absolute paths recorded in transcripts are never opened.
type codebuddyBlobAssets struct {
	home         string
	assetsDir    string
	legacyAssets string
}

// ImageReference returns the asset reference for one attached image. Without
// an asset directory it still computes the content-addressed reference so the
// parse stays deterministic; writing happens only when assetsDir is set.
func (a codebuddyBlobAssets) ImageReference(ref codebuddyImageRef) (string, bool) {
	body, mediaType, ok := a.read(ref)
	if !ok {
		return "", false
	}
	if a.assetsDir == "" {
		asset, err := assets.Reference(mediaType, body)
		return asset, err == nil
	}
	asset, _, err := assets.Put(a.assetsDir, mediaType, body)
	return asset, err == nil
}

func (a codebuddyBlobAssets) read(ref codebuddyImageRef) ([]byte, string, bool) {
	if strings.HasPrefix(ref.dataURL, "data:") {
		return codebuddyDecodeDataURL(ref.dataURL)
	}
	if name, ok := strings.CutPrefix(ref.assetURI, "codebuddy-asset://assets/"); ok {
		return a.readLegacyAsset(name)
	}
	blob := strings.ToLower(ref.blobID)
	if len(blob) != 64 || strings.Trim(blob, "0123456789abcdef") != "" || a.home == "" {
		return nil, "", false
	}
	dir := filepath.Join(a.home, "blobs", blob[:2])
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, "", false
	}
	defer root.Close()
	for _, ext := range codebuddyBlobExtensions(ref.mime) {
		if body, mediaType, ok := codebuddyReadImage(root, blob+ext); ok {
			return body, mediaType, true
		}
	}
	return nil, "", false
}

// readLegacyAsset reads one file by plain name from a converted
// conversation's 4.x assets directory.
func (a codebuddyBlobAssets) readLegacyAsset(name string) ([]byte, string, bool) {
	if a.legacyAssets == "" || name == "" || !filepath.IsLocal(name) ||
		strings.ContainsAny(name, "/\\") {
		return nil, "", false
	}
	root, err := os.OpenRoot(a.legacyAssets)
	if err != nil {
		return nil, "", false
	}
	defer root.Close()
	return codebuddyReadImage(root, name)
}

// codebuddyReadImage reads a bounded image file inside root and reports its
// detected media type.
func codebuddyReadImage(root *os.Root, name string) ([]byte, string, bool) {
	f, err := root.Open(name)
	if err != nil {
		return nil, "", false
	}
	defer f.Close()
	body, err := io.ReadAll(io.LimitReader(f, maxCodeBuddyImageBytes+1))
	if err != nil || len(body) > maxCodeBuddyImageBytes {
		return nil, "", false
	}
	mediaType := http.DetectContentType(body)
	if _, ok := assets.ExtForMediaType(mediaType); !ok {
		return nil, "", false
	}
	return body, mediaType, true
}

// codebuddyBlobExtensions lists the stored extensions to try, preferring the
// one the reference's MIME type names.
func codebuddyBlobExtensions(mime string) []string {
	all := []string{".png", ".jpg", ".jpeg", ".webp", ".gif"}
	preferred := map[string]string{
		"image/png": ".png", "image/jpeg": ".jpg", "image/webp": ".webp",
		"image/gif": ".gif",
	}[strings.ToLower(mime)]
	if preferred == "" {
		return all
	}
	out := []string{preferred}
	for _, ext := range all {
		if ext != preferred {
			out = append(out, ext)
		}
	}
	return out
}

// CodeBuddyReferencedBlobs lists the blob-store files the CBC transcripts
// under projectsRoot reference, so remote sync can carry the images a
// transcript shows without copying the whole store.
func CodeBuddyReferencedBlobs(ctx context.Context, projectsRoot string) ([]string, error) {
	home := filepath.Dir(filepath.Clean(projectsRoot))
	set := newCodeBuddyCBCSourceSet([]string{projectsRoot}, "")
	seen := make(map[string]struct{})
	var out []string
	err := set.DiscoverEach(ctx, func(source SourceRef) error {
		_, path, ok := set.pathFromSource(source)
		if !ok {
			return nil
		}
		ids, err := codebuddyTranscriptBlobIDs(ctx, path)
		if err != nil {
			return err
		}
		for _, blob := range ids {
			if _, dup := seen[blob]; dup {
				continue
			}
			seen[blob] = struct{}{}
			matches, _ := filepath.Glob(filepath.Join(home, "blobs", blob[:2], blob+".*"))
			for _, match := range matches {
				if IsRegularFile(match) {
					out = append(out, match)
				}
			}
		}
		return nil
	})
	slices.Sort(out)
	return out, err
}

var codebuddyBlobRefCache = struct {
	sync.Mutex
	byPath map[string]codebuddyBlobRefs
}{byPath: make(map[string]codebuddyBlobRefs)}

type codebuddyBlobRefs struct {
	size  int64
	mtime int64
	ids   []string
}

// codebuddyTranscriptBlobIDs lists the blob ids one transcript references,
// reusing the previous read while the file's size and modification time are
// unchanged. A transcript removed mid-scan references nothing.
func codebuddyTranscriptBlobIDs(ctx context.Context, path string) ([]string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, nil //nolint:nilerr // A removed transcript references nothing.
	}
	codebuddyBlobRefCache.Lock()
	cached, ok := codebuddyBlobRefCache.byPath[path]
	codebuddyBlobRefCache.Unlock()
	if ok && cached.size == info.Size() && cached.mtime == info.ModTime().UnixNano() {
		return cached.ids, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil //nolint:nilerr // A removed transcript references nothing.
	}
	defer f.Close()
	lr := newLineReaderContext(ctx, f, maxLineSize)
	defer releaseLineReader(lr)
	var ids []string
	for {
		line, ok := lr.next()
		if !ok {
			break
		}
		if !strings.Contains(line, "image_blob_ref") {
			continue
		}
		for _, m := range codebuddyBlobIDRe.FindAllStringSubmatch(line, -1) {
			if !slices.Contains(ids, m[1]) {
				ids = append(ids, m[1])
			}
		}
	}
	if err := lr.Err(); err != nil {
		return nil, err
	}
	codebuddyBlobRefCache.Lock()
	if len(codebuddyBlobRefCache.byPath) >= 1<<16 {
		clear(codebuddyBlobRefCache.byPath)
	}
	codebuddyBlobRefCache.byPath[path] = codebuddyBlobRefs{
		size: info.Size(), mtime: info.ModTime().UnixNano(), ids: ids,
	}
	codebuddyBlobRefCache.Unlock()
	return ids, nil
}

var codebuddyBlobIDRe = regexp.MustCompile(`"blob_id"\s*:\s*"([0-9a-f]{64})"`)

func codebuddyDecodeDataURL(url string) ([]byte, string, bool) {
	header, payload, ok := strings.Cut(strings.TrimPrefix(url, "data:"), ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return nil, "", false
	}
	if base64.StdEncoding.DecodedLen(len(payload)) > maxCodeBuddyImageBytes {
		return nil, "", false
	}
	body, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, "", false
	}
	mediaType := http.DetectContentType(body)
	if _, ok := assets.ExtForMediaType(mediaType); !ok {
		return nil, "", false
	}
	return body, mediaType, true
}
