#!/bin/sh
# Backup of the media directory (./media), the half of the data that pg_dump
# never covers: PostgreSQL stores the METADATA of an attachment (migration
# 0004), the bytes live on disk under ./media/<owner>/<YYYY-MM>/<DD>/<key>.
# A dump alone therefore restores a catalogue of files that no longer exist.
#
# Strategy: WEEKLY FULL + DAILY INCREMENTAL tar.gz archives.
#
# Why archives and not a filesystem snapshot (LVM, btrfs, ZFS): a snapshot is
# the only way to get a genuinely atomic point-in-time view of a tree, but it
# requires the volume to sit on a filesystem that provides it. The homelab VM
# runs on a plain ext4 root without a spare LVM extent, which is the common
# case on a VPS as well; a procedure that cannot be run on the target machine
# is not a backup procedure. tar over an atomically-written tree gets us close
# enough (see "consistency" below) with zero infrastructure requirement, and
# the archives are portable to any offsite target.
#
# Consistency: the downloader (#10) writes each file to a temporary name and
# renames it into place, so a file is either absent or complete -- tar never
# captures a half-written blob. What tar CAN miss is a file that appears after
# its path was listed: that file is simply picked up by the next incremental.
# The resulting discrepancy (a media_files row in 'stored' whose file is not
# yet in any archive) is exactly what the reconciliation command of #12
# reports, and what scripts/restore-media-test.sh proves is detectable.
#
# Coupling with the database dump: an archive is only useful next to the dump
# of the same window. Each archive is written with a `.meta` sidecar naming
# the most recent `undelete-*.sql.gz` present at the time it ran. Restore
# order is DB first, then media, then reconciliation -- see
# docs/backup-restore.md.
#
# This script NEVER deletes anything: no retention purge on media archives,
# unlike scripts/backup.sh. An incremental chain is only restorable as long as
# the full it is based on still exists, so an automated purge is one badly
# chosen predicate away from silently truncating the chain. Retention is
# documented and applied by hand (docs/backup-restore.md).
set -eu

MEDIA_DIR="${MEDIA_DIR:-./media}"
BACKUP_DIR="${BACKUP_DIR:-./backups}"
# auto  : full if no usable full exists, or if the newest one is older than
#         MEDIA_BACKUP_FULL_INTERVAL_DAYS; incremental otherwise.
# full / incremental: force one mode (an incremental with no base fails).
MEDIA_BACKUP_MODE="${MEDIA_BACKUP_MODE:-auto}"
MEDIA_BACKUP_FULL_INTERVAL_DAYS="${MEDIA_BACKUP_FULL_INTERVAL_DAYS:-7}"

if [ ! -d "$MEDIA_DIR" ]; then
    echo "backup-media: media directory not found: $MEDIA_DIR" >&2
    exit 1
fi
mkdir -p "$BACKUP_DIR"

# Both roots are made absolute right away: the listing below runs in a
# subshell that has `cd`-ed into the media directory, where a relative
# ../backups/... reference would resolve somewhere else entirely.
MEDIA_DIR=$(CDPATH= cd -- "$MEDIA_DIR" && pwd)
BACKUP_DIR=$(CDPATH= cd -- "$BACKUP_DIR" && pwd)

# sha256sum on Linux (coreutils) and BusyBox (postgres:16-alpine), shasum on a
# machine that only ships Perl's. Output is normalised to the sha256sum format
# ("<hex>  <path>") in both cases, so a MANIFEST is checkable with
# `sha256sum -c` wherever coreutils exists.
if command -v sha256sum >/dev/null 2>&1; then
    sha256_of() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
    sha256_of() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
    echo "backup-media: neither sha256sum nor shasum available" >&2
    exit 1
fi

started_at="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
timestamp="$(date -u +%Y%m%dT%H%M%SZ)"

# --- Choice of mode and of the base full ------------------------------------
# Archive names embed a UTC timestamp in a format that sorts lexicographically
# in chronological order, so "newest" is the last line of a sorted listing --
# no dependency on `find -printf` or on `stat`, neither of which is portable
# between GNU and BusyBox.
newest_full=$(find "$BACKUP_DIR" -maxdepth 1 -name 'undelete-media-*-full.tar.gz' -type f \
    | sort | tail -n 1)

full_age_days() {
    # Age of the base full, derived from its NAME rather than its mtime: a
    # copy or an rsync to an offsite target rewrites mtime, which would then
    # postpone the next full forever.
    base=$(basename "$1")
    stamp=${base#undelete-media-}
    stamp=${stamp%-full.tar.gz}
    # "20260904T041200Z" -> "2026-09-04" -> days since epoch, computed with
    # `date -d` (GNU) or `date -j` (BusyBox/macOS). If neither works we return
    # an empty age and the caller falls back to taking a full: an extra full
    # costs disk, a missing one costs the chain.
    day="${stamp%T*}"
    iso="$(printf '%s-%s-%s' "$(echo "$day" | cut -c1-4)" "$(echo "$day" | cut -c5-6)" "$(echo "$day" | cut -c7-8)")"
    base_epoch=$(date -u -d "$iso" +%s 2>/dev/null || date -u -j -f '%Y-%m-%d' "$iso" +%s 2>/dev/null || echo '')
    [ -n "$base_epoch" ] || return 1
    now_epoch=$(date -u +%s)
    echo $(( (now_epoch - base_epoch) / 86400 ))
}

mode="$MEDIA_BACKUP_MODE"
case "$mode" in
    full | incremental | auto) ;;
    *)
        echo "backup-media: invalid MEDIA_BACKUP_MODE=$mode (expected auto, full or incremental)" >&2
        exit 1
        ;;
esac

if [ "$mode" = auto ]; then
    if [ -z "$newest_full" ]; then
        mode=full
    else
        age=$(full_age_days "$newest_full" || true)
        if [ -z "$age" ] || [ "$age" -ge "$MEDIA_BACKUP_FULL_INTERVAL_DAYS" ]; then
            mode=full
        else
            mode=incremental
        fi
    fi
fi

if [ "$mode" = incremental ] && [ -z "$newest_full" ]; then
    echo "backup-media: incremental requested but no full archive exists in $BACKUP_DIR." >&2
    echo "backup-media: run once with MEDIA_BACKUP_MODE=full first." >&2
    exit 1
fi

archive="${BACKUP_DIR}/undelete-media-${timestamp}-${mode}.tar.gz"
manifest="${archive%.tar.gz}.manifest"
meta="${archive%.tar.gz}.meta"
checksum="${archive%.tar.gz}.sha256"
skipped="${archive%.tar.gz}.skipped"
# Reference marker of a full: an EMPTY file whose mtime is the instant the
# full STARTED listing. Incrementals select on `-newer` against it, never
# against the archive itself -- the archive's mtime is the instant the full
# FINISHED, and every file created while it ran would fall in the gap and be
# backed up by nothing.
started_marker="${archive%.tar.gz}.started"

# Any interruption (SIGTERM from Compose, full disk, container kill) must not
# leave behind a truncated archive that a future restore would trust. The trap
# removes the whole output set and is lifted only once everything is written.
cleanup_partial() { rm -f "$archive" "$manifest" "$meta" "$checksum" "$skipped" "$started_marker"; }
trap 'cleanup_partial' EXIT HUP INT TERM

workdir=$(mktemp -d)
trap 'cleanup_partial; rm -rf "$workdir"' EXIT HUP INT TERM

echo "backup-media: mode=${mode} media=${MEDIA_DIR} -> $(basename "$archive")"

# --- Listing the files to archive -------------------------------------------
# Paths are relative to $MEDIA_DIR: the archive is extractable over any media
# root, on any machine, without rewriting anything.
if [ "$mode" = full ]; then
    : > "$started_marker"
    base_full='-'
    # find into a file, then post-process: written as `find | sed` the exit
    # status kept would be sed's, and a find that failed halfway (unreadable
    # subdirectory) would produce a short listing presented as a complete
    # backup. There is no `set -o pipefail` here -- this script also runs on
    # the host's dash, which does not have it.
    ( cd "$MEDIA_DIR" && find . -type f ) > "$workdir/raw"
    sed 's|^\./||' "$workdir/raw" | LC_ALL=C sort > "$workdir/files"
else
    base_full=$(basename "$newest_full")
    reference="${newest_full%.tar.gz}.started"
    if [ ! -f "$reference" ]; then
        # The marker is gone (manual cleanup, partial offsite copy). Falling
        # back on the archive itself is less safe -- see the comment on
        # started_marker -- but it still produces a usable incremental, and
        # the fallback is recorded in the .meta so a restore knows.
        echo "backup-media: WARNING: marker $(basename "$reference") missing, falling back to the archive's mtime" >&2
        reference="$newest_full"
    fi
    ( cd "$MEDIA_DIR" && find . -type f -newer "$reference" ) > "$workdir/raw"
    sed 's|^\./||' "$workdir/raw" | LC_ALL=C sort > "$workdir/files"
fi

# A backslash in a path cannot be represented in a sha256sum-format manifest
# (coreutils escapes it and prefixes the line), so such paths are excluded
# here rather than corrupting the manifest silently, and the run ends with a
# non-zero status so cron surfaces it. A path containing a NEWLINE is not
# detectable in a line-based listing at all: it arrives as two lines, neither
# of which names an existing file, and the existence check further down drops
# both with a warning. Nothing the bot writes can produce either shape (paths
# are server-generated, <owner>/YYYY-MM/DD/[A-Za-z0-9_-]): this only fires on
# a file dropped into ./media by hand.
skipped_count=0
# Octal escape rather than a quoted literal: a lone backslash in source reads
# differently depending on the shell (and trips shellcheck's SC1003).
backslash=$(printf '\134')
if LC_ALL=C grep -qF "$backslash" "$workdir/files" 2>/dev/null; then
    LC_ALL=C grep -F "$backslash" "$workdir/files" > "$skipped"
    skipped_count=$(wc -l < "$skipped" | tr -d ' ')
    LC_ALL=C grep -vF "$backslash" "$workdir/files" > "$workdir/files.clean" || true
    mv "$workdir/files.clean" "$workdir/files"
    echo "backup-media: WARNING: ${skipped_count} path(s) excluded, see $(basename "$skipped")" >&2
fi

file_count=$(wc -l < "$workdir/files" | tr -d ' ')
echo "backup-media: ${file_count} file(s) to archive"

# --- MANIFEST: one sha256 per file ------------------------------------------
# Built BEFORE the archive, from the files on disk. It is what makes a restore
# verifiable: after extraction, `sha256sum -c` in the media root proves every
# byte survived the archive, the transfer and the offsite storage. A file that
# disappears between the listing and the hashing (a concurrent purge) is
# dropped from the manifest here, so the tar below never fails on it.
: > "$manifest"
while IFS= read -r rel; do
    [ -n "$rel" ] || continue
    if [ ! -f "${MEDIA_DIR}/${rel}" ]; then
        echo "backup-media: WARNING: ${rel} disappeared while listing, excluded" >&2
        continue
    fi
    printf '%s  %s\n' "$(sha256_of "${MEDIA_DIR}/${rel}")" "$rel" >> "$manifest"
done < "$workdir/files"

# The manifest is authoritative for the archive's content: tar is fed from it,
# not from the raw listing, so archive and manifest cannot disagree.
sed 's/^[0-9a-f]\{64\}  //' "$manifest" > "$workdir/tar-input"
manifest_count=$(wc -l < "$manifest" | tr -d ' ')

# --- The archive -------------------------------------------------------------
# -T reads the member list from a file (supported by GNU tar and by the
# BusyBox tar of postgres:16-alpine). -C makes every member path relative to
# the media root. An empty input is a legitimate case (an incremental on a day
# with no new media): tar then writes a valid, empty archive, which is the
# proof the run happened -- more useful than no file at all.
tar -czf "$archive" -C "$MEDIA_DIR" -T "$workdir/tar-input"

archive_sha=$(sha256_of "$archive")
printf '%s  %s\n' "$archive_sha" "$(basename "$archive")" > "$checksum"

# --- Coupling with the database dump -----------------------------------------
# The newest dump present when this ran. Restoring this archive next to any
# OTHER dump is allowed and expected (an incremental is newer than the dump it
# names), but the pair recorded here is the one the reconciliation of #12 will
# find in its most coherent state.
newest_dump=$(find "$BACKUP_DIR" -maxdepth 1 -name 'undelete-*.sql.gz' -type f | sort | tail -n 1)
if [ -n "$newest_dump" ]; then
    dump_name=$(basename "$newest_dump")
else
    dump_name='-'
    echo "backup-media: WARNING: no undelete-*.sql.gz in ${BACKUP_DIR}; this archive is coupled to no dump" >&2
fi

archive_bytes=$(wc -c < "$archive" | tr -d ' ')
cat > "$meta" <<EOF
schema=1
archive=$(basename "$archive")
mode=${mode}
base_full=${base_full}
db_dump=${dump_name}
media_dir=${MEDIA_DIR}
started_at=${started_at}
finished_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
file_count=${manifest_count}
archive_bytes=${archive_bytes}
archive_sha256=${archive_sha}
manifest=$(basename "$manifest")
skipped_paths=${skipped_count}
EOF

# Everything is on disk and coherent: the partial-output trap can go.
trap 'rm -rf "$workdir"' EXIT HUP INT TERM

echo "backup-media: ${manifest_count} file(s), ${archive_bytes} bytes, sha256 ${archive_sha}"
echo "backup-media: coupled to db dump ${dump_name}"
echo "backup-media: no purge performed (media retention is manual, cf. docs/backup-restore.md)"

if [ "$skipped_count" -gt 0 ]; then
    echo "backup-media: FINISHED WITH WARNINGS -- ${skipped_count} path(s) not archived" >&2
    exit 2
fi
echo "backup-media: done"
