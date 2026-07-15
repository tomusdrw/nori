package registry

import "github.com/google/go-containerregistry/pkg/crane"

// LatestDigest returns the manifest digest (sha256:...) for the given image
// reference. Packages are public, so no auth is configured.
func LatestDigest(ref string, opts ...crane.Option) (string, error) {
	return crane.Digest(ref, opts...)
}
