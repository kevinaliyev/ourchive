-- Phase 4.5: Single-file mounts & fast-hash file pointers for auto-relocation

ALTER TABLE mounts ADD COLUMN kind TEXT NOT NULL DEFAULT 'folder'; -- folder | file
ALTER TABLE share_links ADD COLUMN fast_hash TEXT NOT NULL DEFAULT '';
ALTER TABLE tagged_items ADD COLUMN size_bytes INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tagged_items ADD COLUMN fast_hash TEXT NOT NULL DEFAULT '';
