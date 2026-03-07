package storage

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	benchreport "kvraft/internal/bench/report"
	"kvraft/kvstore"
)

func Run(runDir string, keys, rounds, payloadBytes int, maxDataFileSizeBytes int64, trials int) (benchreport.StorageBenchResult, error) {
	if keys <= 0 || rounds <= 0 || payloadBytes <= 0 {
		return benchreport.StorageBenchResult{}, fmt.Errorf("dataset keys, rounds, and payload bytes must all be > 0")
	}
	if maxDataFileSizeBytes <= 0 {
		return benchreport.StorageBenchResult{}, fmt.Errorf("dataset max data file size must be > 0")
	}
	if trials <= 0 {
		return benchreport.StorageBenchResult{}, fmt.Errorf("restart trials must be > 0")
	}

	datasetSrc := filepath.Join(runDir, "storage_dataset_src")
	if err := os.RemoveAll(datasetSrc); err != nil {
		return benchreport.StorageBenchResult{}, err
	}
	if err := buildSyntheticDataset(datasetSrc, keys, rounds, payloadBytes, maxDataFileSizeBytes); err != nil {
		return benchreport.StorageBenchResult{}, err
	}

	withHints := filepath.Join(runDir, "storage_with_hints")
	noHints := filepath.Join(runDir, "storage_no_hints")
	merged := filepath.Join(runDir, "storage_merged")

	if err := copyDir(datasetSrc, withHints); err != nil {
		return benchreport.StorageBenchResult{}, err
	}
	if err := copyDir(datasetSrc, noHints); err != nil {
		return benchreport.StorageBenchResult{}, err
	}
	if err := copyDir(datasetSrc, merged); err != nil {
		return benchreport.StorageBenchResult{}, err
	}

	if err := removeHintFiles(noHints); err != nil {
		return benchreport.StorageBenchResult{}, err
	}

	preMergeSize, err := dirSizeBytes(merged)
	if err != nil {
		return benchreport.StorageBenchResult{}, err
	}
	if err := mergeDirectory(merged); err != nil {
		return benchreport.StorageBenchResult{}, err
	}
	postMergeSize, err := dirSizeBytes(merged)
	if err != nil {
		return benchreport.StorageBenchResult{}, err
	}

	noHintsSamples, err := measureOpenSamples(noHints, trials)
	if err != nil {
		return benchreport.StorageBenchResult{}, err
	}
	withHintsSamples, err := measureOpenSamples(withHints, trials)
	if err != nil {
		return benchreport.StorageBenchResult{}, err
	}
	mergedSamples, err := measureOpenSamples(merged, trials)
	if err != nil {
		return benchreport.StorageBenchResult{}, err
	}

	sortedNoHints := append([]float64(nil), noHintsSamples...)
	sortedWithHints := append([]float64(nil), withHintsSamples...)
	sortedMerged := append([]float64(nil), mergedSamples...)
	sort.Float64s(sortedNoHints)
	sort.Float64s(sortedWithHints)
	sort.Float64s(sortedMerged)

	restartNoHints := benchreport.Percentile(sortedNoHints, 0.50)
	restartWithHints := benchreport.Percentile(sortedWithHints, 0.50)
	restartMerged := benchreport.Percentile(sortedMerged, 0.50)

	diskReductionPct := 0.0
	if preMergeSize > 0 {
		diskReductionPct = (float64(preMergeSize-postMergeSize) / float64(preMergeSize)) * 100
	}

	restartImprovement := 0.0
	if restartNoHints > 0 {
		restartImprovement = (restartNoHints - restartMerged) / restartNoHints * 100
	}

	return benchreport.StorageBenchResult{
		DatasetDir:             datasetSrc,
		DatasetKeys:            keys,
		DatasetRounds:          rounds,
		DatasetPayloadBytes:    payloadBytes,
		PreMergeSizeBytes:      preMergeSize,
		PostMergeSizeBytes:     postMergeSize,
		DiskReductionPercent:   diskReductionPct,
		RestartNoHintsMS:       restartNoHints,
		RestartWithHintsMS:     restartWithHints,
		RestartMergedHintsMS:   restartMerged,
		RestartImprovementPct:  restartImprovement,
		OpenTrials:             trials,
		OpenNoHintsSamplesMS:   sortedNoHints,
		OpenWithHintsSamplesMS: sortedWithHints,
		OpenMergedSamplesMS:    sortedMerged,
	}, nil
}

func buildSyntheticDataset(dir string, keys, rounds, payloadBytes int, maxDataFileSizeBytes int64) error {
	db, err := kvstore.OpenWithOptions(dir, kvstore.OpenOptions{
		ReadWrite:            true,
		SyncOnPut:            false,
		MaxDataFileSizeBytes: maxDataFileSizeBytes,
	})
	if err != nil {
		return err
	}
	defer db.Close()

	payload := strings.Repeat("v", payloadBytes)
	for r := 0; r < rounds; r++ {
		for k := 0; k < keys; k++ {
			key := fmt.Sprintf("dataset-%08d", k)
			value := fmt.Sprintf("r%02d:%s", r, payload)
			if err := db.Put(key, value); err != nil {
				return err
			}
		}
	}

	return db.Sync()
}

func copyDir(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}

	return filepath.WalkDir(src, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)

		if d.IsDir() {
			info, err := d.Info()
			if err != nil {
				return err
			}
			return os.MkdirAll(target, info.Mode().Perm())
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}

		in, err := os.Open(path)
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			_ = in.Close()
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			_ = in.Close()
			_ = out.Close()
			return err
		}
		if err := in.Close(); err != nil {
			_ = out.Close()
			return err
		}
		return out.Close()
	})
}

func removeHintFiles(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".hint") {
			if err := os.Remove(filepath.Join(dir, entry.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

func mergeDirectory(dir string) error {
	db, err := kvstore.OpenWithOptions(dir, kvstore.OpenOptions{ReadWrite: true})
	if err != nil {
		return err
	}
	defer db.Close()

	if err := db.Merge(); err != nil {
		return err
	}
	return db.Sync()
}

func dirSizeBytes(dir string) (int64, error) {
	var total int64
	err := filepath.WalkDir(dir, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

func measureOpenSamples(dir string, trials int) ([]float64, error) {
	samples := make([]float64, 0, trials)
	for i := 0; i < trials; i++ {
		start := time.Now()
		db, err := kvstore.OpenReadOnly(dir)
		if err != nil {
			return nil, err
		}
		if err := db.Close(); err != nil {
			return nil, err
		}
		samples = append(samples, float64(time.Since(start).Microseconds())/1000.0)
	}
	return samples, nil
}
