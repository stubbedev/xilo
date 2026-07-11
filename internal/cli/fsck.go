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
			sts, err := openStorages(cfg)
			if err != nil {
				return err
			}
			return runFsck(cmd.Context(), cmd, db, sts, content, repair)
		},
	}
	c.Flags().BoolVar(&content, "content", false, "re-hash every blob against its chunk hash (reads all data)")
	c.Flags().BoolVar(&repair, "repair", false, "drop bad chunk rows + broken paths so a re-push heals them")
	return c
}

func runFsck(ctx context.Context, cmd *cobra.Command, db *store.DB, sts map[string]storage.Storage, content, repair bool) error {
	out := cmd.OutOrStdout()
	var dec *zstd.Decoder
	if content {
		var err error
		if dec, err = zstd.NewReader(nil); err != nil {
			return err
		}
		defer dec.Close()
	}

	// badKeys collects "storage/hash" keys; badByStorage feeds per-backend
	// row repair.
	var badKeys []string
	badByStorage := map[string][]string{}
	total := 0
	for name, st := range sts {
		chunks, err := db.AllChunks(name)
		if err != nil {
			return err
		}
		total += len(chunks)
		for _, c := range chunks {
			ok, err := st.Has(ctx, c.Key)
			if err != nil {
				return fmt.Errorf("stat %s: %w", c.Key, err)
			}
			if !ok {
				fmt.Fprintf(out, "MISSING BLOB %s (%s in %s)\n", c.Hash, c.Key, name)
				badKeys = append(badKeys, name+"/"+c.Hash)
				badByStorage[name] = append(badByStorage[name], c.Hash)
				continue
			}
			if content {
				if err := verifyChunkContent(ctx, st, dec, c); err != nil {
					fmt.Fprintf(out, "CORRUPT BLOB %s: %v\n", c.Hash, err)
					badKeys = append(badKeys, name+"/"+c.Hash)
					badByStorage[name] = append(badByStorage[name], c.Hash)
				}
			}
		}
	}

	// Paths whose chunk list references a bad or absent chunk row.
	danglers, err := db.PathsWithMissingChunks(badKeys)
	if err != nil {
		return err
	}
	for _, p := range danglers {
		fmt.Fprintf(out, "BROKEN PATH %s (references missing/corrupt chunks)\n", p.StorePath)
	}

	if len(badKeys) == 0 && len(danglers) == 0 {
		fmt.Fprintf(out, "fsck: %d chunks OK, all path references intact\n", total)
		return nil
	}
	if !repair {
		return fmt.Errorf("fsck: %d bad chunks, %d broken paths (run with --repair to heal)", len(badKeys), len(danglers))
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
	for name, hashes := range badByStorage {
		if err := db.DeleteChunkRows(name, hashes); err != nil {
			return err
		}
	}
	fmt.Fprintf(out, "fsck: repaired — dropped %d chunk rows and %d paths (re-push to restore)\n", len(badKeys), len(danglers))
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
