package main

// Backups: a consistent snapshot (VACUUM INTO), zstd-compressed, uploaded to
// the Hetzner box next to the Anki sync server. Keeps the newest 14 remotely
// and the newest 3 locally.

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

var (
	backupHost   = envOr("MDB_BACKUP_HOST", "root@89.167.79.200")
	backupRemote = envOr("MDB_BACKUP_DIR", "/var/backups/fazon-db")
	keepRemote   = 14
	keepLocal    = 3
)

func cmdBackup(db *sql.DB) error {
	dir := filepath.Join(filepath.Dir(dbPath), "backups")
	os.MkdirAll(dir, 0o755)
	stamp := time.Now().Format("2006-01-02_150405")
	snap := filepath.Join(dir, "fazon-"+stamp+".db")
	t0 := time.Now()
	if _, err := db.Exec(`VACUUM INTO ?`, snap); err != nil {
		return fmt.Errorf("snapshot: %w", err)
	}
	if out, err := exec.Command("zstd", "-q", "-T0", "-6", "--rm", snap).CombinedOutput(); err != nil {
		return fmt.Errorf("zstd: %v %s", err, out)
	}
	zst := snap + ".zst"
	st, _ := os.Stat(zst)
	log.Printf("snapshot %s (%.0f MB) in %s", filepath.Base(zst), float64(st.Size())/1e6, time.Since(t0).Round(time.Second))

	ssh := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=15"}
	if out, err := exec.Command("ssh", append(ssh, backupHost, "mkdir -p "+backupRemote)...).CombinedOutput(); err != nil {
		return fmt.Errorf("ssh: %v %s", err, out)
	}
	if out, err := exec.Command("scp", append(ssh, "-q", zst, backupHost+":"+backupRemote+"/")...).CombinedOutput(); err != nil {
		return fmt.Errorf("scp: %v %s", err, out)
	}
	prune := fmt.Sprintf(`cd %s && ls -1t fazon-*.db.zst | tail -n +%d | xargs -r rm -f; ls -1 fazon-*.db.zst | wc -l; df -h . | tail -1`, backupRemote, keepRemote+1)
	out, err := exec.Command("ssh", append(ssh, backupHost, prune)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("remote prune: %v %s", err, out)
	}
	log.Printf("uploaded to %s:%s (remote: %s)", backupHost, backupRemote, strings.Join(strings.Fields(string(out)), " "))

	// local retention
	files, _ := filepath.Glob(filepath.Join(dir, "fazon-*.db.zst"))
	sort.Strings(files)
	for len(files) > keepLocal {
		os.Remove(files[0])
		files = files[1:]
	}
	setMeta(db, "last_backup", time.Now().Format(time.RFC3339))
	return nil
}
