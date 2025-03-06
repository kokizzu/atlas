// Copyright 2021-present The Atlas Authors. All rights reserved.
// This source code is licensed under the Apache 2.0 license found
// in the LICENSE file in the root directory of this source tree.

//go:build !ent

package cmdext

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	cmdmigrate "ariga.io/atlas/cmd/atlas/internal/migrate"
	"ariga.io/atlas/schemahcl"
	"ariga.io/atlas/sql/migrate"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

var specOptions []schemahcl.Option

// RemoteSchema is a data source that for reading remote schemas.
func RemoteSchema(context.Context, *hcl.EvalContext, *hclsyntax.Block) (cty.Value, error) {
	return cty.Zero, fmt.Errorf("data.remote_schema is not supported by this release. See: https://atlasgo.io/getting-started")
}

// RemoteDir is a data source that reads a remote migration directory.
func RemoteDir(context.Context, *hcl.EvalContext, *hclsyntax.Block) (cty.Value, error) {
	return cty.Zero, fmt.Errorf("data.remote_dir is not supported by this release. See: https://atlasgo.io/getting-started")
}

// StateReaderAtlas returns a migrate.StateReader from an Atlas Cloud schema.
func StateReaderAtlas(context.Context, *StateReaderConfig) (*StateReadCloser, error) {
	return nil, fmt.Errorf("atlas remote state is not supported by this release. See: https://atlasgo.io/getting-started")
}

// SchemaExternal is a data source that for reading external schemas.
func SchemaExternal(context.Context, *hcl.EvalContext, *hclsyntax.Block) (cty.Value, error) {
	return cty.Zero, fmt.Errorf("data.external_schema is no longer supported by this release. See: https://atlasgo.io/getting-started")
}

// EntLoader is a StateLoader for loading ent.Schema's as StateReader's.
type EntLoader struct{}

// LoadState returns a migrate.StateReader that reads the schema from an ent.Schema.
func (l EntLoader) LoadState(context.Context, *StateReaderConfig) (*StateReadCloser, error) {
	return nil, fmt.Errorf("ent:// scheme is no longer supported by this release. See: https://atlasgo.io/getting-started")
}

// MigrateDiff returns the diff between ent.Schema and a directory.
func (l EntLoader) MigrateDiff(context.Context, *MigrateDiffOptions) error {
	return fmt.Errorf("ent:// scheme is no longer supported by this release. See: https://atlasgo.io/getting-started")
}

// InitBlock returns the handler for the "atlas" init block.
func (c *AtlasConfig) InitBlock() schemahcl.Option {
	return schemahcl.WithInitBlock("atlas", func(_ context.Context, ectx *hcl.EvalContext, block *hclsyntax.Block) (cty.Value, error) {
		return cty.NilVal, fmt.Errorf("atlas block is not supported by this release. See: https://atlasgo.io/getting-started")
	})
}

// StateReaderSQL returns a migrate.StateReader from an SQL file or a directory of migrations.
func StateReaderSQL(ctx context.Context, config *StateReaderConfig) (*StateReadCloser, error) {
	if len(config.URLs) != 1 {
		return nil, fmt.Errorf("the provided SQL state must be either a single schema file or a migration directory, but %d paths were found", len(config.URLs))
	}
	var (
		dir  migrate.Dir
		path = filepath.Join(config.URLs[0].Host, config.URLs[0].Path)
	)
	switch fi, err := os.Stat(path); {
	case err != nil:
		return nil, err
	// A single schema file.
	case !fi.IsDir():
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		if bytes.Contains(b, []byte("-- atlas:import ")) {
			return nil, errors.New("atlas:import directive is not supported by this release. See: https://atlasgo.io/getting-started")
		}
		if dir, err = FilesAsDir(migrate.NewLocalFile(fi.Name(), b)); err != nil {
			return nil, err
		}
		return stateSchemaSQL(ctx, config, dir)
	// The sum file is optional when reading the directory state.
	case isSchemaDir(config.URLs[0], path):
		dirs, err := os.ReadDir(path)
		if err != nil {
			return nil, err
		}
		files := make([]migrate.File, 0, len(dirs))
		for _, d := range dirs {
			b, err := os.ReadFile(filepath.Join(path, d.Name()))
			if err != nil {
				return nil, err
			}
			if bytes.Contains(b, []byte("-- atlas:import ")) {
				return nil, errors.New("atlas:import directive is not supported by this release. See: https://atlasgo.io/getting-started")
			}
			files = append(files, migrate.NewLocalFile(d.Name(), b))
		}
		if dir, err = FilesAsDir(files...); err != nil {
			return nil, err
		}
		return stateSchemaSQL(ctx, config, dir)
	// A migration directory.
	default:
		var opts []migrate.ReplayOption
		if dir, err = cmdmigrate.DirURL(ctx, config.URLs[0], false); err != nil {
			return nil, err
		}
		if v := config.URLs[0].Query().Get("version"); v != "" {
			opts = append(opts, migrate.ReplayToVersion(v))
		}
		return stateReaderSQL(ctx, config, dir, nil, opts)
	}
}
