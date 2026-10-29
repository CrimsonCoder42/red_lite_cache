-- ================================
-- CLUSTER: the fencing epoch (D-012)
-- ================================

-- name: GetEpoch :one
SELECT epoch FROM cluster_state
WHERE id = 1;

-- name: RaiseEpoch :one
-- Stores epoch if it is higher than the stored one and returns the epoch now stored. A promoted
-- replica and a booting leader both call it; the epoch never goes backwards.
INSERT INTO cluster_state (id, epoch, instance_id, updated_at)
VALUES (1, @epoch, @instance_id, now())
ON CONFLICT (id) DO UPDATE
SET epoch = GREATEST(cluster_state.epoch, EXCLUDED.epoch),
    instance_id = CASE WHEN EXCLUDED.epoch > cluster_state.epoch THEN EXCLUDED.instance_id ELSE cluster_state.instance_id END,
    updated_at = now()
RETURNING epoch;
