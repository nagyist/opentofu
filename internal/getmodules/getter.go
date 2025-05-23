// Copyright (c) The OpenTofu Authors
// SPDX-License-Identifier: MPL-2.0
// Copyright (c) 2023 HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package getmodules

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"

	cleanhttp "github.com/hashicorp/go-cleanhttp"
	getter "github.com/hashicorp/go-getter"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/opentofu/opentofu/internal/copy"
)

// We configure our own go-getter detector and getter sets here, because
// the set of sources we support is part of OpenTofu's documentation and
// so we don't want any new sources introduced in go-getter to sneak in here
// and work even though they aren't documented. This also insulates us from
// any meddling that might be done by other go-getter callers linked into our
// executable.
//
// Note that over time we've found go-getter's design to be not wholly fit
// for OpenTofu's purposes in various ways, and so we're continuing to use
// it here because our backward compatibility with earlier versions depends
// on it, but we use go-getter very carefully and always only indirectly via
// the public API of this package so that we can get the subset of the
// go-getter functionality we need while working around some of the less
// helpful parts of its design. See the comments in various other functions
// in this package which call into go-getter for more information on what
// tradeoffs we're making here.

var goGetterDetectors = []getter.Detector{
	&withoutQueryParams{d: new(getter.GitHubDetector)},
	new(getter.GitDetector),

	// Because historically BitBucket supported both Git and Mercurial
	// repositories but used the same repository URL syntax for both,
	// this detector takes the unusual step of actually reaching out
	// to the BitBucket API to recognize the repository type. That
	// means there's the possibility of an outgoing network request
	// inside what is otherwise normally just a local string manipulation
	// operation, but we continue to accept this for now.
	//
	// Perhaps a future version of go-getter will remove the check now
	// that BitBucket only supports Git anyway. Aside from this historical
	// exception, we should avoid adding any new detectors that make network
	// requests in here, and limit ourselves only to ones that can operate
	// entirely through local string manipulation.
	new(getter.BitBucketDetector),

	new(getter.GCSDetector),
	new(getter.S3Detector),
	new(fileDetector),
}

var goGetterNoDetectors = []getter.Detector{}

var goGetterDecompressors = map[string]getter.Decompressor{
	"bz2": new(getter.Bzip2Decompressor),
	"gz":  new(getter.GzipDecompressor),
	"xz":  new(getter.XzDecompressor),
	"zip": new(getter.ZipDecompressor),

	"tar.bz2":  new(getter.TarBzip2Decompressor),
	"tar.tbz2": new(getter.TarBzip2Decompressor),

	"tar.gz": new(getter.TarGzipDecompressor),
	"tgz":    new(getter.TarGzipDecompressor),

	"tar.xz": new(getter.TarXzDecompressor),
	"txz":    new(getter.TarXzDecompressor),
}

// This is a map from media types as used in OCI descriptors to the keys in
// [goGetterDecompressors] whose decompressor can be used for each type.
//
// This intentionally covers only the subset of archive formats we support
// for module packages retrieved from OCI distribution repositories, but
// is defined in here so we can more easily correlate it with
// goGetterDecompressors when adding new entries.
//
// If this map grows in future then any new keys must also appear somewhere
// in [ociBlobMediaTypePreference].
var goGetterDecompressorMediaTypes = map[string]string{
	"archive/zip": "zip",
}

// goGetterGetters is an initial table of getters that we use as a starting
// point when building a _real_ table of getters to pass into a
// [reusingGetter] instance.
//
// The elements mapped to nil here are those which are populated dynamically
// based on arguments to [NewPackageFetcher], included here only so it's
// easier to refer to the entire list of supported getter keys in one place.
var goGetterGetters = map[string]getter.Getter{
	"file":  new(getter.FileGetter),
	"gcs":   new(getter.GCSGetter),
	"git":   new(getter.GitGetter),
	"hg":    new(getter.HgGetter),
	"http":  getterHTTPGetter,
	"https": getterHTTPGetter,
	"oci":   nil, // configured dynamically using [PackageFetcherEnvironment.OCIRepositoryStore]
	"s3":    new(getter.S3Getter),
}

var getterHTTPClient = &http.Client{
	Transport: otelhttp.NewTransport(cleanhttp.DefaultTransport()),
}

var getterHTTPGetter = &getter.HttpGetter{
	Client:             getterHTTPClient,
	Netrc:              true,
	XTerraformGetLimit: 10,
}

// A reusingGetter is a helper for the module installer that remembers
// the final resolved addresses of all of the sources it has already been
// asked to install, and will copy from a prior installation directory if
// it has the same resolved source address.
//
// The keys in a reusingGetter are the normalized (post-detection) package
// addresses, and the values are the paths where each source was previously
// installed. (Users of this map should treat the keys as addrs.ModulePackage
// values, but we can't type them that way because the addrs package
// imports getmodules in order to indirectly access our go-getter
// configuration.)
type reusingGetter struct {
	// getters are the go-getter getters that this particular instance of
	// reusingGetter should use.
	getters map[string]getter.Getter

	previousInstalls   map[string]string // initialized on first install request
	previousInstallsMu sync.Mutex        // must hold while interacting with previousInstalls
}

func newReusingGetter(getters map[string]getter.Getter) *reusingGetter {
	return &reusingGetter{
		getters: getters,
		// previousInstalls initialized only on request
	}
}

// getWithGoGetter fetches the package at the given address into the given
// target directory. The given address must already be in normalized form
// (using NormalizePackageAddress) or else the behavior is undefined.
//
// This function deals only in entire packages, so it's always the caller's
// responsibility to handle any subdirectory specification and select a
// suitable subdirectory of the given installation directory after installation
// has succeeded.
//
// This function would ideally accept packageAddr as a value of type
// addrs.ModulePackage, but we can't do that because the addrs package
// depends on this package for package address parsing. Therefore we just
// use a string here but assume that the caller got that value by calling
// the String method on a valid addrs.ModulePackage value.
//
// The errors returned by this function are those surfaced by the underlying
// go-getter library, which have very inconsistent quality as
// end-user-actionable error messages. At this time we do not have any
// reasonable way to improve these error messages at this layer because
// the underlying errors are not separately recognizable.
func (g *reusingGetter) getWithGoGetter(ctx context.Context, instPath, packageAddr string) error {
	var err error

	// For now we hold the "previousInstalls" mutex throughout our entire work here
	// since we don't currently try to use a single getter concurrently anyway.
	// If we _do_ want to enable more concurrency in future then we'll need a
	// more interesting strategy to make sure that only concurrent attempts to
	// install the _same_ package get serialized, but we'll wait until we have
	// that need before we introduce that complexity.
	//
	// NOTE WELL: [getter.Client.Get] modifies internal state inside each of the
	// getters passed in [getter.Client.Getters] before calling into them, so
	// it is _not_ safe to reuse the same getter instances across multiple
	// concurrent calls. If we want to make this work concurrently in future
	// then we'll need to instead instantiate the getters on demand for each
	// request.
	g.previousInstallsMu.Lock()
	defer g.previousInstallsMu.Unlock()
	if g.previousInstalls == nil {
		g.previousInstalls = make(map[string]string)
	}

	if prevDir, exists := g.previousInstalls[packageAddr]; exists {
		log.Printf("[TRACE] getmodules: copying previous install of %q from %s to %s", packageAddr, prevDir, instPath)
		err := os.Mkdir(instPath, os.ModePerm)
		if err != nil {
			return fmt.Errorf("failed to create directory %s: %w", instPath, err)
		}
		err = copy.CopyDir(instPath, prevDir)
		if err != nil {
			return fmt.Errorf("failed to copy from %s to %s: %w", prevDir, instPath, err)
		}
	} else {
		log.Printf("[TRACE] getmodules: fetching %q to %q", packageAddr, instPath)
		client := getter.Client{
			Src: packageAddr,
			Dst: instPath,
			Pwd: instPath,

			Mode: getter.ClientModeDir,

			Detectors:     goGetterNoDetectors, // our caller should've already done detection
			Decompressors: goGetterDecompressors,
			Getters:       g.getters,
			Ctx:           ctx,
		}
		err = client.Get()
		if err != nil {
			return err
		}
		// Remember where we installed this so we might reuse this directory
		// on subsequent calls to avoid re-downloading.
		g.previousInstalls[packageAddr] = instPath
	}

	// If we get down here then we've either downloaded the package or
	// copied a previous tree we downloaded, and so either way we should
	// have got the full module package structure written into instPath.
	return nil
}

// withoutQueryParams implements getter.Detector and can be used to wrap another detector.
// This will look for any query params that might exist in the src and strip that away before calling
// getter.Detector#Detect. After the response is returned, the query params are attached back to the resulted src.
type withoutQueryParams struct {
	d getter.Detector
}

func (w *withoutQueryParams) Detect(src string, pwd string) (string, bool, error) {
	var qp string
	if idx := strings.Index(src, "?"); idx > -1 {
		qp = src[idx+1:]
		src = src[:idx]
	}

	src, ok, err := w.d.Detect(src, pwd)
	// Attach the query params only when the wrapped detector returns a value back
	if len(src) > 0 && len(qp) > 0 {
		src += "?" + qp
	}
	return src, ok, err
}
