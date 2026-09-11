// Copyright (C) 2026 Eric <lizhi.xmu@gmail.com>
//
// This file is part of rclone and is released under the MIT License.
// See the LICENSE file in the repository root for details.

// Package drive115 implements a read-only backend for 115 cloud storage
// (https://115.com) on top of the 115driver library.
//
// Design notes (why it looks like this):
//
//   - 115 is addressed by numeric ids (cid for directories, fid for files),
//     while rclone is path based.  The backend keeps a process wide cache of
//     path -> cid and cid -> listing so that a normal rclone mount only needs
//     roughly one API request per directory.  Paths are resolved in a single
//     request with the `files/getid` API (DirName2CID) and directory listings
//     back-fill the cid of every child directory they return.
//
//   - Directory listings use the low level GetFiles call rather than
//     client.List so that the total count is available (exact pagination) and
//     every page can be rate limited individually.  client.List would issue
//     many pages back to back inside a single rate limit slot.
//
//   - All 115 API traffic is funnelled through one process wide lock plus a
//     minimum interval (default 500ms ~= 2 QPS) because 115's WAF rate limits
//     clients and the 115driver client is not safe for concurrent use.  The
//     actual file download goes straight to the CDN and is not serialised.
//
//   - If the WAF does block us (it returns an HTML page instead of JSON) the
//     backend trips a global cooldown instead of hammering the API.
//
// All mutating operations return errorReadOnly.
package drive115

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/SheltonZhu/115driver/pkg/driver"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fshttp"
	"github.com/rclone/rclone/fs/hash"
)

const (
	// rootID is the 115 cid of the root directory.
	rootID = "0"
	// defaultQPS is the default maximum number of 115 API requests per second.
	defaultQPS = 2.0
	// minQPS is the lowest allowed request rate. A configured qps below this
	// value is clamped up to it so the mount stays usable.
	minQPS = 1.5
	// defaultPageSize is the number of entries fetched per listing request.
	defaultPageSize = int64(1000)
	// maxPageSize is the largest page size 115 accepts (driver.MaxDirPageLimit).
	maxPageSize = int64(1150)
	// defaultListCacheTime is how long path/cid and listing caches live.
	defaultListCacheTime = time.Hour
	// defaultDownloadCacheTime is how long a signed download URL is reused.
	defaultDownloadCacheTime = 5 * time.Minute
	// defaultAPITimeout is the HTTP timeout for 115 API requests.  Without it a
	// request blocked by the WAF can hang forever and wedge the FUSE mount.
	defaultAPITimeout = 60 * time.Second
	// wafCooldown is how long to stop calling the API after a WAF block.
	wafCooldown = 10 * time.Minute
)

var errorReadOnly = errors.New("115 backend is read only")

// defaultListAPIURLs are tried in order when listing directories.  115's WAF
// may block one endpoint for a source IP while the others keep working, so the
// backend remembers which one succeeded and falls back through the rest.  The
// last entry is plain HTTP (that host serves no HTTPS) and is a last resort.
var defaultListAPIURLs = fs.CommaSepList{
	"https://aps.115.com/natsort/files.php",
	"https://webapi.115.com/files",
	"http://web.api.115.com/files",
}

func init() {
	fsi := &fs.RegInfo{
		Name:        "115",
		Description: "115 Cloud Storage (read-only)",
		NewFs:       NewFs,
		Options: []fs.Option{{
			Name:     "uid",
			Help:     "UID from the 115 cookie.",
			Required: true,
		}, {
			Name:     "cid",
			Help:     "CID from the 115 cookie.",
			Required: true,
		}, {
			Name:       "seid",
			Help:       "SEID from the 115 cookie.",
			Required:   true,
			IsPassword: true,
		}, {
			Name:       "kid",
			Help:       "KID from the 115 cookie (optional).",
			IsPassword: true,
		}, {
			Name:     "qps",
			Help:     "Maximum number of 115 API requests per second.\n\nThe value is clamped to a minimum of 1.5 requests/second; the default is 2.",
			Default:  defaultQPS,
			Advanced: true,
		}, {
			Name:     "api_timeout",
			Help:     "HTTP timeout for 115 API requests.\n\nWithout a timeout a request blocked by 115's WAF can hang forever and wedge the mount.",
			Default:  fs.Duration(defaultAPITimeout),
			Advanced: true,
		}, {
			Name:     "page_size",
			Help:     "Number of entries requested per directory listing call.\n\nLarger values mean fewer requests for big directories.",
			Default:  defaultPageSize,
			Advanced: true,
		}, {
			Name:     "list_cache_time",
			Help:     "How long to cache path resolutions and directory listings in memory. Set to 0 to disable caching.",
			Default:  fs.Duration(defaultListCacheTime),
			Advanced: true,
		}, {
			Name:     "download_cache_time",
			Help:     "How long to reuse a signed download URL for the same file.\n\n115 download URLs stay valid for a while, so re-opening the same file shortly after (a second read stream, a retry, a rescan) reuses the URL instead of asking the API for a new one. Set to 0 to disable.",
			Default:  fs.Duration(defaultDownloadCacheTime),
			Advanced: true,
		}, {
			Name:     "list_api_urls",
			Help:     "Endpoints used for directory listings, tried in order until one succeeds.",
			Default:  defaultListAPIURLs,
			Advanced: true,
		}},
	}
	fs.Register(fsi)
}

// Options defines the configuration for this backend.
type Options struct {
	UID               string          `config:"uid"`
	CID               string          `config:"cid"`
	SEID              string          `config:"seid"`
	KID               string          `config:"kid"`
	QPS               float64         `config:"qps"`
	APITimeout        fs.Duration     `config:"api_timeout"`
	PageSize          int64           `config:"page_size"`
	ListCacheTime     fs.Duration     `config:"list_cache_time"`
	DownloadCacheTime fs.Duration     `config:"download_cache_time"`
	ListAPIURLs       fs.CommaSepList `config:"list_api_urls"`
}

// ---------------------------------------------------------------------------
// Process wide shared state
// ---------------------------------------------------------------------------

var (
	// apiMu serialises every 115 API call. 115driver's client stores the last
	// request on the client itself, so it must not be used concurrently.
	apiMu sync.Mutex
	// apiLast is when the previous API call started, used to enforce the
	// minimum interval.
	apiLast time.Time

	// wafMu guards wafUntil.
	wafMu    sync.Mutex
	wafUntil time.Time
)

// apiEnter serialises access to the 115 API, enforces the minimum interval and
// refuses to make calls while a WAF cooldown is active.
// qpsToInterval converts a requests-per-second setting into the minimum
// interval between calls, clamping the rate to at least minQPS.
func qpsToInterval(qps float64) time.Duration {
	if qps < minQPS {
		qps = minQPS
	}
	return time.Duration(float64(time.Second) / qps)
}

func apiEnter(ctx context.Context, minInterval time.Duration) error {
	apiMu.Lock()

	wafMu.Lock()
	until := wafUntil
	wafMu.Unlock()
	if !until.IsZero() && time.Now().Before(until) {
		apiMu.Unlock()
		return fmt.Errorf("115: WAF cooldown active until %s", until.Format(time.RFC3339))
	}

	if minInterval > 0 {
		if d := minInterval - time.Since(apiLast); d > 0 {
			select {
			case <-time.After(d):
			case <-ctx.Done():
				apiMu.Unlock()
				return ctx.Err()
			}
		}
	}
	apiLast = time.Now()
	return nil
}

func apiExit() { apiMu.Unlock() }

func markWAFBlocked() {
	wafMu.Lock()
	wafUntil = time.Now().Add(wafCooldown)
	wafMu.Unlock()
}

// isWAFError reports whether an error looks like an Aliyun WAF block page
// rather than a normal 115 API error.
func isWAFError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "<title>405</title>") ||
		strings.Contains(s, "安全威胁") ||
		strings.Contains(s, "aliyun") ||
		strings.Contains(s, "blocked")
}

// sharedClient keeps a single 115driver client (and cookie check) for the
// process so that multiple Fs instances do not each log in.
var sharedClient struct {
	mu      sync.Mutex
	key     string
	client  *driver.Pan115Client
	checked bool
}

func clientFor(opt *Options) (*driver.Pan115Client, error) {
	key := opt.UID + "|" + opt.CID + "|" + opt.SEID + "|" + opt.KID

	sharedClient.mu.Lock()
	defer sharedClient.mu.Unlock()
	if sharedClient.client != nil && sharedClient.key == key && sharedClient.checked {
		return sharedClient.client, nil
	}

	if err := apiEnter(context.Background(), qpsToInterval(opt.QPS)); err != nil {
		return nil, err
	}
	defer apiExit()

	c := driver.Default()
	// Bound every 115 API request so a WAF block cannot hang the mount forever.
	c.Client.SetTimeout(time.Duration(opt.APITimeout))
	c.ImportCredential(&driver.Credential{
		UID:  opt.UID,
		CID:  opt.CID,
		SEID: opt.SEID,
		KID:  opt.KID,
	})
	if err := c.CookieCheck(); err != nil {
		return nil, fmt.Errorf("115: cookie check failed: %w", err)
	}

	sharedClient.client = c
	sharedClient.key = key
	sharedClient.checked = true
	return c, nil
}

// dirCacheEntry is a cached directory listing for a single cid.
type dirCacheEntry struct {
	files []driver.File
	time  time.Time
}

// gcache holds the process wide caches.
var gcache = struct {
	mu      sync.Mutex
	dirs    map[string]*dirCacheEntry // cid -> listing
	paths   map[string]string         // "a/b" -> cid
	listURL string                    // last listing endpoint that worked
}{
	dirs:  map[string]*dirCacheEntry{},
	paths: map[string]string{},
}

// downloadInfoCacheEntry is a cached signed download URL for one file.
type downloadInfoCacheEntry struct {
	info *driver.DownloadInfo
	ua   string
	time time.Time
}

// dlCache caches signed download URLs by pickcode. 115 download URLs stay
// valid for a while, so re-opening the same file soon after (a second read
// stream, a retry, a media scanner probing the same file) reuses the URL
// instead of asking the API for a new one. This keeps the number of requests
// to 115's download-URL endpoint down, which is the endpoint its WAF limits.
var dlCache = struct {
	mu    sync.Mutex
	items map[string]*downloadInfoCacheEntry
}{items: map[string]*downloadInfoCacheEntry{}}

func dlCacheGet(pickCode, ua string, ttl time.Duration) *driver.DownloadInfo {
	if ttl <= 0 {
		return nil
	}
	dlCache.mu.Lock()
	defer dlCache.mu.Unlock()
	e, ok := dlCache.items[pickCode]
	if !ok || e.ua != ua || time.Since(e.time) >= ttl {
		return nil
	}
	return e.info
}

func dlCachePut(pickCode, ua string, info *driver.DownloadInfo) {
	dlCache.mu.Lock()
	dlCache.items[pickCode] = &downloadInfoCacheEntry{info: info, ua: ua, time: time.Now()}
	dlCache.mu.Unlock()
}

func dlCacheDel(pickCode string) {
	dlCache.mu.Lock()
	delete(dlCache.items, pickCode)
	dlCache.mu.Unlock()
}

// ---------------------------------------------------------------------------
// Fs
// ---------------------------------------------------------------------------

// Fs represents a remote 115 filesystem.
type Fs struct {
	name       string
	root       string
	features   *fs.Features
	opt        Options
	client     *driver.Pan115Client
	httpClient *http.Client
}

// Object describes a 115 file.
type Object struct {
	fs       *Fs
	remote   string
	fileID   string
	pickCode string
	sha1     string
	size     int64
	modTime  time.Time
}

// NewFs constructs an Fs from the path.
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	if err := configstruct.Set(m, opt); err != nil {
		return nil, err
	}
	if opt.QPS <= 0 {
		opt.QPS = defaultQPS
	}
	if opt.QPS < minQPS {
		opt.QPS = minQPS
	}
	if opt.APITimeout == 0 {
		opt.APITimeout = fs.Duration(defaultAPITimeout)
	}
	if opt.PageSize <= 0 {
		opt.PageSize = defaultPageSize
	}
	if opt.PageSize > maxPageSize {
		opt.PageSize = maxPageSize
	}
	if opt.ListCacheTime == 0 {
		opt.ListCacheTime = fs.Duration(defaultListCacheTime)
	}
	if opt.DownloadCacheTime == 0 {
		opt.DownloadCacheTime = fs.Duration(defaultDownloadCacheTime)
	}
	if len(opt.ListAPIURLs) == 0 {
		opt.ListAPIURLs = defaultListAPIURLs
	}
	if opt.UID == "" || opt.CID == "" || opt.SEID == "" {
		return nil, errors.New("115: uid, cid and seid are required")
	}

	client, err := clientFor(opt)
	if err != nil {
		return nil, err
	}

	f := &Fs{
		name:       name,
		root:       root,
		opt:        *opt,
		client:     client,
		httpClient: fshttp.NewClient(ctx),
	}
	f.features = (&fs.Features{
		CanHaveEmptyDirectories: true,
	}).Fill(ctx, f)

	fs.Debugf(f, "115: qps=%g (min interval %v), api_timeout=%v, page_size=%d, list_cache_time=%v, download_cache_time=%v",
		opt.QPS, qpsToInterval(opt.QPS), time.Duration(opt.APITimeout), opt.PageSize, time.Duration(opt.ListCacheTime), time.Duration(opt.DownloadCacheTime))

	// Resolve the root.  An empty root is the common mount case and costs no
	// requests at all.
	if root != "" {
		entry, err := f.lookupEntry(ctx, root)
		if err == nil && !entry.IsDirectory {
			f.root = path.Dir(root)
			if f.root == "." || f.root == "/" {
				f.root = ""
			}
			return f, fs.ErrorIsFile
		}
	}

	return f, nil
}

// Name returns the configured name of the file system.
func (f *Fs) Name() string { return f.name }

// Root returns the root for the filesystem.
func (f *Fs) Root() string { return f.root }

// String returns a description of the file system.
func (f *Fs) String() string { return "115 root '" + f.root + "'" }

// Features returns the optional features of this Fs.
func (f *Fs) Features() *fs.Features { return f.features }

// Precision returns the modtime precision of the 115 filesystem.
func (f *Fs) Precision() time.Duration { return time.Second }

// Hashes returns the supported hash types of the filesystem. 115 exposes SHA1.
func (f *Fs) Hashes() hash.Set { return hash.NewHashSet(hash.SHA1) }

// withAPI runs fn with exclusive access to the 115 API and enforces the
// configured minimum interval.
func (f *Fs) withAPI(ctx context.Context, fn func() error) error {
	if err := apiEnter(ctx, qpsToInterval(f.opt.QPS)); err != nil {
		return err
	}
	defer apiExit()
	return fn()
}

// absPath converts a path relative to the rclone root into an absolute path
// within the 115 account (which is what the caches and the API use).
func (f *Fs) absPath(remote string) string {
	remote = strings.Trim(remote, "/")
	switch {
	case f.root == "":
		return remote
	case remote == "":
		return f.root
	default:
		return f.root + "/" + remote
	}
}

// orderedURLs returns the listing endpoints to try, with the last known good
// one first.
func (f *Fs) orderedURLs() []string {
	gcache.mu.Lock()
	preferred := gcache.listURL
	gcache.mu.Unlock()

	urls := make([]string, 0, len(f.opt.ListAPIURLs)+1)
	if preferred != "" {
		urls = append(urls, preferred)
	}
	for _, u := range f.opt.ListAPIURLs {
		if u != preferred {
			urls = append(urls, u)
		}
	}
	return urls
}

// listDir returns the (cached) listing for a directory cid. dirPath is the
// path the cid corresponds to and is used to back-fill the path -> cid cache
// for child directories.
func (f *Fs) listDir(ctx context.Context, cid, dirPath string) ([]driver.File, error) {
	ttl := time.Duration(f.opt.ListCacheTime)
	if ttl > 0 {
		gcache.mu.Lock()
		if entry, ok := gcache.dirs[cid]; ok && time.Since(entry.time) < ttl {
			files := entry.files
			gcache.mu.Unlock()
			return files, nil
		}
		gcache.mu.Unlock()
	}

	var all []driver.File
	offset := int64(0)
	urls := f.orderedURLs()

	for {
		var resp *driver.FileListResp
		var lastErr error
		for _, u := range urls {
			url := u
			err := f.withAPI(ctx, func() error {
				// Use Client.R() directly: Pan115Client.NewRequest() writes
				// the request to the shared client and would race.  The
				// ForceContentType is required - GetFiles relies on the
				// caller having set it (client.List does the same).
				var e error
				resp, e = driver.GetFiles(
					f.client.Client.R().ForceContentType("application/json;charset=UTF-8"),
					cid,
					driver.WithApiURL(url),
					driver.WithLimit(f.opt.PageSize),
					driver.WithOffset(offset),
				)
				return e
			})
			if err == nil {
				gcache.mu.Lock()
				gcache.listURL = url
				gcache.mu.Unlock()
				lastErr = nil
				break
			}
			lastErr = err
			fs.Debugf(f, "115: listing via %s failed: %v", url, err)
			if isWAFError(err) {
				markWAFBlocked()
				fs.Errorf(f, "115: WAF blocked listing via %s, cooling down for %s", url, wafCooldown)
				break
			}
		}
		if lastErr != nil {
			return nil, lastErr
		}

		for i := range resp.Files {
			file := (&driver.File{}).From(&resp.Files[i])
			all = append(all, *file)
			if file.IsDirectory {
				child := file.Name
				if dirPath != "" {
					child = dirPath + "/" + file.Name
				}
				gcache.mu.Lock()
				gcache.paths[child] = file.FileID
				gcache.mu.Unlock()
			}
		}

		n := int64(len(resp.Files))
		offset += n
		if n == 0 || offset >= int64(resp.Count) {
			break
		}
	}

	if ttl > 0 {
		gcache.mu.Lock()
		gcache.dirs[cid] = &dirCacheEntry{files: all, time: time.Now()}
		gcache.mu.Unlock()
	}
	return all, nil
}

// resolveDir turns a path relative to the root into a cid.  It uses the fast
// `files/getid` API first (one request for the whole path) and falls back to
// walking the tree with listings, which also back-fills the cid cache.
func (f *Fs) resolveDir(ctx context.Context, dir string) (string, error) {
	dir = strings.Trim(dir, "/")
	if dir == "" {
		return rootID, nil
	}

	gcache.mu.Lock()
	if cid, ok := gcache.paths[dir]; ok {
		gcache.mu.Unlock()
		return cid, nil
	}
	gcache.mu.Unlock()

	// Fast path: one request for the whole path.
	var resp *driver.APIGetDirIDResp
	err := f.withAPI(ctx, func() error {
		var e error
		resp, e = f.client.DirName2CID(dir)
		return e
	})
	if err == nil && string(resp.CategoryID) != "" && string(resp.CategoryID) != rootID {
		cid := string(resp.CategoryID)
		gcache.mu.Lock()
		gcache.paths[dir] = cid
		gcache.mu.Unlock()
		return cid, nil
	}
	if isWAFError(err) {
		markWAFBlocked()
		return "", err
	}

	// Fallback: walk from the root one component at a time.
	id := rootID
	prefix := ""
	for _, comp := range strings.Split(dir, "/") {
		if comp == "" {
			continue
		}
		files, err := f.listDir(ctx, id, prefix)
		if err != nil {
			return "", err
		}
		found := false
		for i := range files {
			if files[i].IsDirectory && files[i].Name == comp {
				id = files[i].FileID
				found = true
				break
			}
		}
		if !found {
			return "", fs.ErrorDirNotFound
		}
		if prefix == "" {
			prefix = comp
		} else {
			prefix = prefix + "/" + comp
		}
	}
	gcache.mu.Lock()
	gcache.paths[dir] = id
	gcache.mu.Unlock()
	return id, nil
}

// lookupEntry resolves a full remote path to the 115 entry it names.
func (f *Fs) lookupEntry(ctx context.Context, remote string) (*driver.File, error) {
	dir, name := path.Split(remote)
	dir = strings.Trim(dir, "/")
	cid, err := f.resolveDir(ctx, dir)
	if err != nil {
		return nil, err
	}
	files, err := f.listDir(ctx, cid, dir)
	if err != nil {
		return nil, err
	}
	for i := range files {
		if files[i].Name == name {
			return &files[i], nil
		}
	}
	return nil, fs.ErrorObjectNotFound
}

func (f *Fs) newObject(remote string, file *driver.File) *Object {
	return &Object{
		fs:       f,
		remote:   remote,
		fileID:   file.FileID,
		pickCode: file.PickCode,
		sha1:     file.Sha1,
		size:     file.Size,
		modTime:  file.UpdateTime,
	}
}

// List the objects and directories in dir into entries.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	abs := f.absPath(dir)
	cid, err := f.resolveDir(ctx, abs)
	if err != nil {
		return nil, err
	}
	files, err := f.listDir(ctx, cid, abs)
	if err != nil {
		return nil, err
	}
	dirPath := strings.Trim(dir, "/")
	entries := make(fs.DirEntries, 0, len(files))
	for i := range files {
		file := &files[i]
		remote := file.Name
		if dirPath != "" {
			remote = dirPath + "/" + file.Name
		}
		if file.IsDirectory {
			entries = append(entries, fs.NewDir(remote, file.UpdateTime))
		} else {
			entries = append(entries, f.newObject(remote, file))
		}
	}
	return entries, nil
}

// NewObject finds the Object at remote.
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	entry, err := f.lookupEntry(ctx, f.absPath(remote))
	if err != nil {
		return nil, err
	}
	if entry.IsDirectory {
		return nil, fs.ErrorIsDir
	}
	return f.newObject(remote, entry), nil
}

// About returns quota information for the account.
func (f *Fs) About(ctx context.Context) (*fs.Usage, error) {
	var info driver.InfoData
	err := f.withAPI(ctx, func() error {
		var e error
		info, e = f.client.GetInfo()
		return e
	})
	if err != nil {
		return nil, err
	}
	total := info.SpaceInfo.AllTotal.Size
	used := info.SpaceInfo.AllUse.Size
	free := info.SpaceInfo.AllRemain.Size
	return &fs.Usage{
		Total: &total,
		Used:  &used,
		Free:  &free,
	}, nil
}

// Put is not supported - the backend is read only.
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return nil, errorReadOnly
}

// Mkdir is not supported - the backend is read only.
func (f *Fs) Mkdir(ctx context.Context, dir string) error { return errorReadOnly }

// Rmdir is not supported - the backend is read only.
func (f *Fs) Rmdir(ctx context.Context, dir string) error { return errorReadOnly }

// Fs returns read only access to the Fs that this object is part of.
func (o *Object) Fs() fs.Info { return o.fs }

// String returns a description of the Object.
func (o *Object) String() string {
	if o == nil {
		return "<nil>"
	}
	return o.remote
}

// Remote returns the remote path.
func (o *Object) Remote() string { return o.remote }

// Hash returns the selected checksum of the file.  rclone hashes are
// lower-case hex, so the value must not be upper-cased here.
func (o *Object) Hash(ctx context.Context, ty hash.Type) (string, error) {
	if ty == hash.SHA1 {
		return strings.ToLower(o.sha1), nil
	}
	return "", hash.ErrUnsupported
}

// Size returns the size of the file.
func (o *Object) Size() int64 { return o.size }

// ModTime returns the modification date of the file.
func (o *Object) ModTime(ctx context.Context) time.Time { return o.modTime }

// Storable says whether this object can be stored.
func (o *Object) Storable() bool { return true }

// Open opens the file for read and returns a reader for the requested range.
//
// 115 does not allow random access directly: the client asks the API for a
// short-lived signed CDN URL for the file's pickcode.  That URL's signature is
// bound to the User-Agent used to request it, so the URL is requested with the
// exact User-Agent that will later fetch it (rclone's own User-Agent, which
// rclone's fshttp transport forces onto every request).  This mirrors what
// OpenList does in drivers/115/driver.go Link().
//
// Signed URLs are cached for download_cache_time so that repeated opens of the
// same file (a second read stream, a retry, a scanner probing it) do not each
// cost a request to 115's download-URL endpoint.
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	ua := fs.GetConfig(ctx).UserAgent
	ttl := time.Duration(o.fs.opt.DownloadCacheTime)

	info := dlCacheGet(o.pickCode, ua, ttl)
	fromCache := info != nil
	if fromCache {
		fs.Debugf(o, "reusing cached download URL")
	}

	// Two attempts: the first may use a cached (possibly stale) URL; if the CDN
	// rejects it we drop the cache entry and sign a fresh URL.
	for attempt := 0; attempt < 2; attempt++ {
		if info == nil {
			var err error
			info, err = o.fetchDownloadURL(ctx, ua)
			if err != nil {
				return nil, err
			}
			if ttl > 0 {
				dlCachePut(o.pickCode, ua, info)
			}
		}

		res, err := o.download(ctx, info, options...)
		if err == nil {
			return res, nil
		}

		var httpErr *downloadHTTPError
		if fromCache && errors.As(err, &httpErr) &&
			(httpErr.status == http.StatusForbidden || httpErr.status == http.StatusNotFound) {
			fs.Debugf(o, "cached download URL rejected (%d), refreshing", httpErr.status)
			dlCacheDel(o.pickCode)
			info = nil
			fromCache = false
			continue
		}
		return nil, err
	}
	return nil, errors.New("115: download failed after refreshing URL")
}

// fetchDownloadURL asks the API for a signed, User-Agent bound download URL.
func (o *Object) fetchDownloadURL(ctx context.Context, ua string) (*driver.DownloadInfo, error) {
	var info *driver.DownloadInfo
	err := o.fs.withAPI(ctx, func() error {
		var e error
		info, e = o.fs.client.DownloadWithUA(o.pickCode, ua)
		return e
	})
	if err != nil {
		if isWAFError(err) {
			markWAFBlocked()
		}
		return nil, err
	}
	if info == nil || info.Url.Url == "" {
		return nil, errors.New("115: empty download URL")
	}
	return info, nil
}

// downloadHTTPError is a non-2xx response from the 115 CDN.
type downloadHTTPError struct {
	status int
	body   string
}

func (e *downloadHTTPError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("115: download failed: HTTP %d", e.status)
	}
	return fmt.Sprintf("115: download failed: HTTP %d: %s", e.status, e.body)
}

// download performs the CDN GET using a signed download URL.
func (o *Object) download(ctx context.Context, info *driver.DownloadInfo, options ...fs.OpenOption) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.Url.Url, nil)
	if err != nil {
		return nil, err
	}
	if info.Header != nil {
		req.Header = info.Header.Clone()
	}
	// These belong to the API request and must not be forwarded to the CDN GET.
	req.Header.Del("Content-Length")
	req.Header.Del("Content-Type")
	req.Header.Del("Host")
	// Translate rclone's range/seek options into HTTP headers.
	for k, v := range fs.OpenOptionHeaders(options) {
		req.Header.Set(k, v)
	}
	// Do NOT override the User-Agent: the URL was signed with rclone's
	// User-Agent and fshttp will force exactly that onto the wire. Setting a
	// different one (or none) makes 115's CDN reject the signature.

	res, err := o.fs.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 1024))
		_ = res.Body.Close()
		return nil, &downloadHTTPError{status: res.StatusCode, body: strings.TrimSpace(string(body))}
	}
	return res.Body, nil
}

// SetModTime is not supported - the backend is read only.
func (o *Object) SetModTime(ctx context.Context, t time.Time) error { return errorReadOnly }

// Update is not supported - the backend is read only.
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	return errorReadOnly
}

// Remove is not supported - the backend is read only.
func (o *Object) Remove(ctx context.Context) error { return errorReadOnly }

// Check the interfaces are satisfied.
var (
	_ fs.Fs      = (*Fs)(nil)
	_ fs.Object  = (*Object)(nil)
	_ fs.Abouter = (*Fs)(nil)
)
