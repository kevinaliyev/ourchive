-- Phase 4: Support folder-as-ZIP share links alongside single-file shares

ALTER TABLE share_links ADD COLUMN is_dir INTEGER NOT NULL DEFAULT 0;
