// Package harborclient stores and retrieves CLI deploy archives in the
// in-cluster Harbor registry as single-layer OCI artifacts (push, pull, and
// list under the runos-archives project), and lazily creates that project on
// first push.
package harborclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/memory"
	"oras.land/oras-go/v2/registry/remote"
	"oras.land/oras-go/v2/registry/remote/auth"
	"oras.land/oras-go/v2/registry/remote/retry"
)

const (
	ArchivesProject = "runos-archives"
	ArtifactType    = "application/vnd.runos.cli-archive+gzip"
	LayerMediaType  = "application/vnd.oci.image.layer.v1.tar+gzip"

	// harborHTTPTimeout bounds the REST calls (list / ensure project) so a
	// hung Harbor can't block an instruction indefinitely.
	harborHTTPTimeout = 60 * time.Second
	// harborErrBodyLimit caps how much of an error response body we read into
	// an error message.
	harborErrBodyLimit = 64 << 10 // 64 KiB
)

// httpClient is used for Harbor's plain REST API (list / ensure project). The
// ORAS push/pull paths use their own retry client and are left unchanged.
var httpClient = &http.Client{Timeout: harborHTTPTimeout}

// Config holds the Harbor connection parameters.
type Config struct {
	URL      string
	Username string
	Password string
}

// Archive describes a stored CLI archive entry returned by ListArchives.
type Archive struct {
	CliUploadID string    `json:"cliUploadId"`
	Digest      string    `json:"digest"`
	Size        int64     `json:"size"`
	PushTime    time.Time `json:"pushTime"`
}

func host(cfg Config) string {
	h := strings.TrimPrefix(cfg.URL, "https://")
	h = strings.TrimPrefix(h, "http://")
	return strings.TrimSuffix(h, "/")
}

func newRepo(cfg Config, osid string) (*remote.Repository, error) {
	h := host(cfg)
	repo, err := remote.NewRepository(fmt.Sprintf("%s/%s/%s", h, ArchivesProject, osid))
	if err != nil {
		return nil, err
	}
	repo.Client = &auth.Client{
		Client: retry.DefaultClient,
		Cache:  auth.NewCache(),
		Credential: auth.StaticCredential(h, auth.Credential{
			Username: cfg.Username,
			Password: cfg.Password,
		}),
	}
	return repo, nil
}

// PushArchive packs a gzipped tarball as a single-layer OCI artifact and pushes
// it to {harbor}/runos-archives/{osid}:{cliUploadID}. The Harbor project is created
// lazily on first call.
func PushArchive(ctx context.Context, cfg Config, osid, cliUploadID string, tarball []byte) error {
	if err := ensureProject(ctx, cfg); err != nil {
		return fmt.Errorf("ensure project: %w", err)
	}
	repo, err := newRepo(cfg, osid)
	if err != nil {
		return fmt.Errorf("new repository: %w", err)
	}
	store := memory.New()
	layerDesc, err := oras.PushBytes(ctx, store, LayerMediaType, tarball)
	if err != nil {
		return fmt.Errorf("push layer to memory: %w", err)
	}
	manifestDesc, err := oras.PackManifest(ctx, store, oras.PackManifestVersion1_1, ArtifactType, oras.PackManifestOptions{
		Layers: []ocispec.Descriptor{layerDesc},
	})
	if err != nil {
		return fmt.Errorf("pack manifest: %w", err)
	}
	if err := store.Tag(ctx, manifestDesc, cliUploadID); err != nil {
		return fmt.Errorf("tag manifest: %w", err)
	}
	if _, err := oras.Copy(ctx, store, cliUploadID, repo, cliUploadID, oras.DefaultCopyOptions); err != nil {
		return fmt.Errorf("copy to remote: %w", err)
	}
	return nil
}

// PullArchive resolves the manifest at {osid}:{cliUploadID}, fetches the single
// gzipped tar layer, and streams its bytes into w.
func PullArchive(ctx context.Context, cfg Config, osid, cliUploadID string, w io.Writer) error {
	repo, err := newRepo(cfg, osid)
	if err != nil {
		return fmt.Errorf("new repository: %w", err)
	}
	manifestDesc, err := repo.Resolve(ctx, cliUploadID)
	if err != nil {
		return fmt.Errorf("resolve manifest: %w", err)
	}
	rc, err := repo.Fetch(ctx, manifestDesc)
	if err != nil {
		return fmt.Errorf("fetch manifest: %w", err)
	}
	manifestBytes, err := io.ReadAll(rc)
	rc.Close()
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	var manifest ocispec.Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("parse manifest: %w", err)
	}
	if len(manifest.Layers) == 0 {
		return fmt.Errorf("manifest has no layers")
	}
	layerRC, err := repo.Fetch(ctx, manifest.Layers[0])
	if err != nil {
		return fmt.Errorf("fetch layer: %w", err)
	}
	defer layerRC.Close()
	if _, err := io.Copy(w, layerRC); err != nil {
		return fmt.Errorf("stream layer: %w", err)
	}
	return nil
}

// ListArchives queries Harbor's v2 REST API for all artifacts under the OSID
// repository and returns one entry per (artifact, tag) pair.
func ListArchives(ctx context.Context, cfg Config, osid string) ([]Archive, error) {
	apiURL := fmt.Sprintf("%s/api/v2.0/projects/%s/repositories/%s/artifacts?with_tag=true&page_size=100",
		strings.TrimSuffix(cfg.URL, "/"), ArchivesProject, osid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.SetBasicAuth(cfg.Username, cfg.Password)
	req.Header.Set("Accept", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return []Archive{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, harborErrBodyLimit))
		return nil, fmt.Errorf("harbor list returned %d: %s", resp.StatusCode, string(body))
	}

	var raw []struct {
		Digest   string    `json:"digest"`
		Size     int64     `json:"size"`
		PushTime time.Time `json:"push_time"`
		Tags     []struct {
			Name string `json:"name"`
		} `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("decode harbor response: %w", err)
	}

	out := make([]Archive, 0, len(raw))
	for _, a := range raw {
		for _, t := range a.Tags {
			out = append(out, Archive{
				CliUploadID: t.Name,
				Digest:      a.Digest,
				Size:        a.Size,
				PushTime:    a.PushTime,
			})
		}
	}
	return out, nil
}

// ensureProject creates the Harbor project if it does not already exist.
// 201 Created and 409 Conflict are both treated as success.
func ensureProject(ctx context.Context, cfg Config) error {
	apiURL := strings.TrimSuffix(cfg.URL, "/") + "/api/v2.0/projects"
	payload := bytes.NewBufferString(`{"project_name":"` + ArchivesProject + `","metadata":{"public":"false"}}`)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, payload)
	if err != nil {
		return err
	}
	req.SetBasicAuth(cfg.Username, cfg.Password)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusConflict {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, harborErrBodyLimit))
	return fmt.Errorf("create project returned %d: %s", resp.StatusCode, string(body))
}
