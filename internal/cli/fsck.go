package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"

	"github.com/klauspost/compress/zstd"
	"github.com/spf13/cobra"

	"github.com/stubbedev/xilo/internal/storage"
	"github.com/stubbedev/xilo/internal/store"
)

func fsckCmd() *cobra.Command {
	var repair, content bool
	c := &cobra.Command{
		Use:   "fsck",
		Short: "Verify chunk rows against stored blobs (and paths against chunks)",
		Long: "Verify the metadata database against the chunk store.\n\n" +
			"Detects the states normal operation can't heal: a chunk row whose blob is\n" +
			"missing or corrupt (crash, disk damage) is trusted by dedup forever and\n" +
			"breaks every NAR that references it. --content re-hashes every blob\n" +
			"(slow, reads all data); the default checks existence only.\n" +
			"--repair drops bad chunk rows and the paths referencing them, so the\n" +
			"next push re-uploads the data.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, db, err := openDB()
			if err != nil {
				return err
			}
			defer db.Close()
			st, err := storage.New(cfg.Storage)
			if err != nil {
				return err
			}
			return runFsck(cmd.Context(), cmd, db, st, content, repair)
		},
	}
	c.Flags().BoolVar(&content, "content", false, "re-hash every blob against its chunk hash (reads all data)")
	c.Flags().BoolVar(&repair, "repair", false, "drop bad chunk rows + broken paths so a re-push heals them")
	return c
}

func runFsck(ctx context.Context, cmd *cobra.Command, db *store.DB, st storage.Storage, content, repair bool) error {
	out := cmd.OutOrStdout()
	chunks, err := db.AllChunks()
	if err != nil {
		return err
	}
	var dec *zstd.Decoder
	if content {
		if dec, err = zstd.NewReader(nil); err != nil {
			return err
		}
		defer dec.Close()
	}

	var bad []string
	for _, c := range chunks {
		ok, err := st.Has(ctx, c.Key)
		if err != nil {
			return fmt.Errorf("stat %s: %w", c.Key, err)
		}
		if !ok {
			fmt.Fprintf(out, "MISSING BLOB %s (%s)\n", c.Hash, c.Key)
			bad = append(bad, c.Hash)
			continue
		}
		if content {
			if err := verifyChunkContent(ctx, st, dec, c); err != nil {
				fmt.Fprintf(out, "CORRUPT BLOB %s: %v\n", c.Hash, err)
				bad = append(bad, c.Hash)
			}
		}
	}

	// Paths whose chunk list references a bad or absent chunk row.
	danglers, err := db.PathsWithMissingChunks(bad)
	if err != nil {
		return err
	}
	for _, p := range danglers {
		fmt.Fprintf(out, "BROKEN PATH %s (references missing/corrupt chunks)\n", p.StorePath)
	}

	if len(bad) == 0 && len(danglers) == 0 {
		fmt.Fprintf(out, "fsck: %d chunks OK, all path references intact\n", len(chunks))
		return nil
	}
	if !repair {
		return fmt.Errorf("fsck: %d bad chunks, %d broken paths (run with --repair to heal)", len(bad), len(danglers))
	}

	// Heal: drop broken paths first (nothing may reference the rows we are
	// about to delete), then the bad chunk rows. The next push re-uploads both.
	ids := make([]int64, len(danglers))
	for i, p := range danglers {
		ids[i] = p.ID
	}
	if err := db.DeletePaths(ids); err != nil {
		return err
	}
	if err := db.DeleteChunkRows(bad); err != nil {
		return err
	}
	fmt.Fprintf(out, "fsck: repaired — dropped %d chunk rows and %d paths (re-push to restore)\n", len(bad), len(danglers))
	return nil
}

// verifyChunkContent decompresses a blob and checks it hashes to the row's
// content address (chunk hashes are sha256 hex of the raw bytes).
func verifyChunkContent(ctx context.Context, st storage.Storage, dec *zstd.Decoder, c store.ChunkRef) error {
	rc, err := st.Get(ctx, c.Key)
	if err != nil {
		return err
	}
	defer rc.Close()
	compressed, err := io.ReadAll(rc)
	if err != nil {
		return err
	}
	raw, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		return fmt.Errorf("zstd: %w", err)
	}
	sum := sha256.Sum256(raw)
	if got := hex.EncodeToString(sum[:]); got != c.Hash {
		return fmt.Errorf("content hash %s", got[:12])
	}
	return nil
}
