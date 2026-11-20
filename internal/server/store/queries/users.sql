-- name: GetUserByID :one
SELECT * FROM users WHERE id = $1;

-- name: GetUserByUsername :one
SELECT * FROM users WHERE username = $1;

-- name: GetUserByHCAID :one
SELECT * FROM users WHERE hca_id = $1;

-- name: InsertAuditLog :exec
INSERT INTO audit_log (actor_user_id, action, subject_type, subject_id, data)
VALUES ($1, $2, $3, $4, $5);
