-- Manual "featured" curation for the viewer browse hero billboard, kept in its
-- own table rather than a column on titles so the catalog stays untouched. An
-- admin hand-picks which titles appear in the hero and in what order (position,
-- ascending). Rows cascade away if the title is deleted.
CREATE TABLE IF NOT EXISTS featured_titles (
    title_id   BIGINT    NOT NULL PRIMARY KEY,
    position   INT       NOT NULL,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    CONSTRAINT fk_featured_title FOREIGN KEY (title_id) REFERENCES titles (id) ON DELETE CASCADE
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci;
