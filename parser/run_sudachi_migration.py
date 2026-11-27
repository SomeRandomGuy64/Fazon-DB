#!/usr/bin/env python3
"""Install a tested binary, snapshot the local DB, parse, and restore the service.

Runs without a terminal; status.json and parse.log are retained beside the local
snapshot. Never invokes the remote-uploading `mdb backup` command.
"""
import argparse
from datetime import datetime
import json
import os
from pathlib import Path
import shutil
import sqlite3
import subprocess
from zoneinfo import ZoneInfo

ROOT = Path(__file__).resolve().parents[1]


def main():
    ap = argparse.ArgumentParser(description=__doc__)
    ap.add_argument('--job-dir', type=Path, required=True)
    ap.add_argument('--use-installed', action='store_true', help='reparse with the current executable')
    ap.add_argument('--force', action='store_true', help='reparse all notes, including already current notes')
    ap.add_argument('--jobs', type=int, default=2, choices=[1, 2])
    args = ap.parse_args()
    job = args.job_dir.resolve()
    job.mkdir(parents=True, exist_ok=True)
    status_file = job / 'status.json'
    if status_file.exists():
        raise RuntimeError('Use a new job directory; existing migration state is retained')
    status = dict(state='starting', started=datetime.now(ZoneInfo('Europe/London')).isoformat(),
                  pid=os.getpid(), jobs=args.jobs)

    def save(**updates):
        status.update(updates)
        temp = job / 'status.json.tmp'
        temp.write_text(json.dumps(status, indent=2) + '\n')
        temp.replace(status_file)

    save()
    binary, built = ROOT / 'mdb', ROOT / 'mdb.sudachi'
    dbfile = ROOT / 'data/fazon.db'
    was_active = subprocess.run(['systemctl', '--user', 'is-active', '--quiet', 'fazon-db.service']).returncode == 0
    stopped = False
    try:
        if not args.use_installed and not built.is_file():
            raise RuntimeError('Build mdb.sudachi and run checks before installing')
        if was_active:
            subprocess.run(['systemctl', '--user', 'stop', 'fazon-db.service'], check=True)
            stopped = True
        save(state='snapshotting', service_was_active=was_active)
        shutil.copy2(binary, job / 'mdb.before_sudachi')
        src = sqlite3.connect(dbfile.as_uri() + '?mode=ro', uri=True)
        dest = sqlite3.connect(job / 'fazon.before_sudachi.sqlite')
        src.backup(dest)
        dest.close()
        status['notes_total'] = src.execute('SELECT COUNT(*) FROM notes').fetchone()[0]
        src.close()
        if not args.use_installed:
            built.replace(binary)
        save(state='parsing', backup=str(job / 'fazon.before_sudachi.sqlite'))
        with (job / 'parse.log').open('w') as log:
            result = subprocess.run([str(binary), 'reparse' if args.force else 'parse', '-j', str(args.jobs)], cwd=ROOT,
                                    stdout=log, stderr=subprocess.STDOUT)
        if result.returncode:
            save(state='failed', returncode=result.returncode, resume_command=f'{binary} parse -j {args.jobs}')
        else:
            save(state='complete', finished=datetime.now(ZoneInfo('Europe/London')).isoformat())
    except BaseException as error:
        save(state='failed', error=str(error))
        raise
    finally:
        if stopped:
            restored = subprocess.run(['systemctl', '--user', 'start', 'fazon-db.service'])
            save(service_restored=restored.returncode == 0)


if __name__ == '__main__':
    main()
