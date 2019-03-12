/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package containerd

import (
	"context"
	"log"
	"runtime"

	"github.com/containerd/containerd/errdefs"
	"github.com/containerd/containerd/images"
	"github.com/containerd/containerd/platforms"
	"github.com/containerd/containerd/remotes"
	"github.com/containerd/containerd/remotes/docker"
	"github.com/containerd/containerd/remotes/docker/schema1"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	"github.com/pkg/errors"
	"golang.org/x/sync/semaphore"
)

// Pull downloads the provided content into containerd's content store
// and returns a platform specific image object
func (c *Client) Pull(ctx context.Context, ref string, opts ...RemoteOpt) (Image, error) {
	pullCtx := defaultRemoteContext()
	for _, o := range opts {
		if err := o(c, pullCtx); err != nil {
			return nil, err
		}
	}
	log.Println("os: " + runtime.GOOS)
	log.Println("arch: " + runtime.GOARCH)

	log.Println("-----------------------------------Pull 1")
	log.Println("pullCtx.PlatformMatcher")
	log.Println(pullCtx.PlatformMatcher)
	log.Println("len(pullCtx.Platforms)")
	log.Println(len(pullCtx.Platforms))

	if pullCtx.PlatformMatcher == nil {
		if len(pullCtx.Platforms) > 1 {
			return nil, errors.New("cannot pull multiplatform image locally, try Fetch")
		} else if len(pullCtx.Platforms) == 0 {
			pullCtx.PlatformMatcher = platforms.Default()
			log.Println("pullCtx.PlatformMatcher 2")
			log.Println(pullCtx.PlatformMatcher)
		} else {
			p, err := platforms.Parse(pullCtx.Platforms[0])
			if err != nil {
				return nil, errors.Wrapf(err, "invalid platform %s", pullCtx.Platforms[0])
			}

			pullCtx.PlatformMatcher = platforms.Only(p)
		}
	}

	ctx, done, err := c.WithLease(ctx)
	if err != nil {
		return nil, err
	}
	defer done(ctx)

	img, err := c.fetch(ctx, pullCtx, ref, 1)
	if err != nil {
		return nil, err
	}

	log.Println("------------------------obainted image from fetch")
	log.Println("name: " + img.Name)
	log.Println("created at: ", img.CreatedAt)
	log.Println("updated at: ", img.UpdatedAt)
	log.Println("--------------------printing image Target")
	log.Println("media type: ", img.Target.MediaType)
	log.Println("platform: ", img.Target.Platform)
	log.Println("digest: ", img.Target.Digest)
	log.Println("size: ", img.Target.Size)
	log.Println("annotations: ", img.Target.Annotations)
	log.Println("urls: ", img.Target.URLs)

	// i is just a containerd image wrapper on the images.Image
	// with platform and the containerd client
	i := NewImageWithPlatform(c, img, pullCtx.PlatformMatcher)

	if pullCtx.Unpack {
		if err := i.Unpack(ctx, pullCtx.Snapshotter); err != nil {
			return nil, errors.Wrapf(err, "failed to unpack image on snapshotter %s", pullCtx.Snapshotter)
		}
	}

	return i, nil
}

func (c *Client) fetch(ctx context.Context, rCtx *RemoteContext, ref string, limit int) (images.Image, error) {
	store := c.ContentStore()
	// Resolve makes are remote request (get manifest by tag/sha) to get the descriptor
	name, desc, err := rCtx.Resolver.Resolve(ctx, ref)

	log.Println("-----------------------------------fetch 1")
	log.Println("name: " + name)
	log.Println("desc digest: " + desc.Digest)

	if err != nil {
		return images.Image{}, errors.Wrapf(err, "failed to resolve reference %q", ref)
	}

	fetcher, err := rCtx.Resolver.Fetcher(ctx, name)
	if err != nil {
		return images.Image{}, errors.Wrapf(err, "failed to get fetcher for %q", name)
	}

	var (
		handler images.Handler

		isConvertible bool
		converterFunc func(context.Context, ocispec.Descriptor) (ocispec.Descriptor, error)
		limiter       *semaphore.Weighted
	)

	log.Println("desc media type: " + desc.MediaType)
	// Legacy Docker schema1 manifest
	if desc.MediaType == images.MediaTypeDockerSchema1Manifest && rCtx.ConvertSchema1 {
		schema1Converter := schema1.NewConverter(store, fetcher)

		handler = images.Handlers(append(rCtx.BaseHandlers, schema1Converter)...)

		isConvertible = true

		converterFunc = func(ctx context.Context, _ ocispec.Descriptor) (ocispec.Descriptor, error) {
			return schema1Converter.Convert(ctx)
		}
	} else {
		// Get all the children for a descriptor
		childrenHandler := images.ChildrenHandler(store)
		// Set any children labels for that content
		childrenHandler = images.SetChildrenLabels(store, childrenHandler)
		// Filter manifests by platforms but allow to handle manifest
		// and configuration for not-target platforms
		childrenHandler = remotes.FilterManifestByPlatformHandler(childrenHandler, rCtx.PlatformMatcher)
		// Sort and limit manifests if a finite number is needed
		if limit > 0 {
			childrenHandler = images.LimitManifests(childrenHandler, rCtx.PlatformMatcher, limit)
		}

		// set isConvertible to true if there is application/octet-stream media type
		convertibleHandler := images.HandlerFunc(
			func(_ context.Context, desc ocispec.Descriptor) ([]ocispec.Descriptor, error) {
				if desc.MediaType == docker.LegacyConfigMediaType {
					isConvertible = true
				}

				return []ocispec.Descriptor{}, nil
			},
		)

		handlers := append(rCtx.BaseHandlers,
			remotes.FetchHandler(store, fetcher),
			convertibleHandler,
			childrenHandler,
		)

		// append distribution source label to blob data
		if rCtx.AppendDistributionSourceLabel {
			appendDistSrcLabelHandler, err := docker.AppendDistributionSourceLabel(store, ref)
			if err != nil {
				return images.Image{}, err
			}

			handlers = append(handlers, appendDistSrcLabelHandler)
		}

		handler = images.Handlers(handlers...)

		converterFunc = func(ctx context.Context, desc ocispec.Descriptor) (ocispec.Descriptor, error) {
			return docker.ConvertManifest(ctx, store, desc)
		}
	}

	if rCtx.HandlerWrapper != nil {
		handler = rCtx.HandlerWrapper(handler)
	}

	if rCtx.MaxConcurrentDownloads > 0 {
		limiter = semaphore.NewWeighted(int64(rCtx.MaxConcurrentDownloads))
	}

	if err := images.Dispatch(ctx, handler, limiter, desc); err != nil {
		return images.Image{}, err
	}

	if isConvertible {
		if desc, err = converterFunc(ctx, desc); err != nil {
			return images.Image{}, err
		}
	}

	img := images.Image{
		Name:   name,
		Target: desc,
		Labels: rCtx.Labels,
	}

	is := c.ImageService()
	for {
		if created, err := is.Create(ctx, img); err != nil {
			if !errdefs.IsAlreadyExists(err) {
				log.Println("error NOT due to already exists")
				return images.Image{}, err
			}

			log.Println("error IS due to already exists")
			updated, err := is.Update(ctx, img)
			if err != nil {
				// if image was removed, try create again
				if errdefs.IsNotFound(err) {
					continue
				}
				return images.Image{}, err
			}

			img = updated
		} else {
			img = created
		}

		return img, nil
	}
}
