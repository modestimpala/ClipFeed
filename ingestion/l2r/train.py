"""
Learning-to-rank training script.
Trains a LightGBM LGBMRanker on interaction data and exports to custom JSON format.
"""

import json
import logging
import os
from datetime import datetime, timezone

import numpy as np
from lightgbm import LGBMRanker
from .features import FEATURE_NAMES, extract_features

log = logging.getLogger(__name__)

DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")
MODEL_PATH = "/data/l2r_model.json"
MIN_SAMPLES = 50


def _tree_structure_to_flat(node: dict) -> list[dict]:
    """
    Recursively convert LightGBM tree_structure to flat array format.
    Returns list of node dicts with root at index 0 for standard traversal.
    """
    if "leaf_value" in node:
        return [{
            "feature_index": -1,
            "threshold": 0.0,
            "left_child": -1,
            "right_child": -1,
            "leaf_value": float(node["leaf_value"]),
            "is_leaf": True,
        }]

    left_child = node.get("left_child", {})
    right_child = node.get("right_child", {})
    left_list = _tree_structure_to_flat(left_child)
    right_list = _tree_structure_to_flat(right_child)
    left_idx = 1
    right_idx = 1 + len(left_list)
    split_feature = node.get("split_feature", node.get("split_index", 0))
    threshold = float(node.get("threshold", 0.0))

    root_node = {
        "feature_index": int(split_feature),
        "threshold": threshold,
        "left_child": left_idx,
        "right_child": right_idx,
        "leaf_value": 0.0,
        "is_leaf": False,
    }
    return [root_node] + left_list + right_list


def _ndcg_at_k(y_true: np.ndarray, y_score: np.ndarray, group_sizes: np.ndarray, k: int = 10) -> float:
    """Compute mean NDCG@k across groups."""
    from sklearn.metrics import ndcg_score
    if len(group_sizes) == 0 or group_sizes.sum() == 0:
        return 0.0
    ndcgs = []
    offset = 0
    for g in group_sizes:
        if g == 0:
            continue
        yg = y_true[offset : offset + g]
        sg = y_score[offset : offset + g]
        offset += g
        if g < 2:
            continue
        try:
            ndcgs.append(ndcg_score([yg], [sg], k=min(k, g)))
        except Exception:
            continue
    return float(np.mean(ndcgs)) if ndcgs else 0.0


def _split_by_groups(
    X: np.ndarray, y: np.ndarray, group_sizes: np.ndarray, test_size: float = 0.2, seed: int = 42
) -> tuple:
    """
    Split data by whole groups (users) to avoid leakage.

    Single-user fallback: when n_groups == 1, a random group split is
    impossible (100% lands in one bucket). Instead, do a chronological
    time-series split — train on the first (1-test_size) interactions,
    test on the remainder. The data from extract_features is already
    ordered by (user_id, created_at), so temporal order is preserved.
    """
    n_groups = len(group_sizes)

    if n_groups <= 1:
        n_samples = len(X)
        split_idx = max(1, int(n_samples * (1 - test_size)))
        split_idx = min(split_idx, n_samples - 1)
        return (
            X[:split_idx],
            y[:split_idx],
            np.array([split_idx], dtype=np.int32),
            X[split_idx:],
            y[split_idx:],
            np.array([n_samples - split_idx], dtype=np.int32),
        )

    rng = np.random.default_rng(seed)
    group_ids = np.arange(n_groups)
    rng.shuffle(group_ids)
    n_test = max(1, int(n_groups * test_size))
    test_groups = set(group_ids[:n_test])
    train_groups = set(group_ids[n_test:])

    train_idx, test_idx = [], []
    offset = 0
    for i, g in enumerate(group_sizes):
        if i in train_groups:
            train_idx.extend(range(offset, offset + g))
        else:
            test_idx.extend(range(offset, offset + g))
        offset += g

    g_train = np.array([group_sizes[i] for i in sorted(train_groups)], dtype=np.int32)
    g_test = np.array([group_sizes[i] for i in sorted(test_groups)], dtype=np.int32)
    return (
        X[train_idx],
        y[train_idx],
        g_train,
        X[test_idx],
        y[test_idx],
        g_test,
    )


def main() -> None:
    """Run training pipeline and export model."""
    logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")

    X, y, group_sizes = extract_features(DB_PATH)
    if len(X) < MIN_SAMPLES:
        log.warning(
            "Insufficient samples: %d (need at least %d). Exiting.",
            len(X),
            MIN_SAMPLES,
        )
        return

    (
        X_train,
        y_train,
        g_train,
        X_test,
        y_test,
        g_test,
    ) = _split_by_groups(X, y, group_sizes, test_size=0.2)

    # LightGBM LambdaRank expects integer relevance labels.
    # Our labels are {0.0, 0.5, 1.0}, so map to graded relevance {0, 1, 2}.
    y_train_rank = np.clip(np.rint(y_train * 2.0), 0, 2).astype(np.int32)

    model = LGBMRanker(
        objective="lambdarank",
        n_estimators=100,
        num_leaves=31,
        learning_rate=0.1,
        label_gain=[0, 1, 2],
        random_state=42,
    )
    model.fit(X_train, y_train_rank, group=g_train)

    y_pred = model.predict(X_test)
    ndcg = _ndcg_at_k(y_test, y_pred, g_test, k=10)
    log.info("Test NDCG@10: %.4f", ndcg)

    dump = model.booster_.dump_model()
    tree_info = dump.get("tree_info", [])
    trees_flat = []
    for ti in tree_info:
        ts = ti.get("tree_structure", ti) if isinstance(ti, dict) else {}
        if isinstance(ts, dict):
            trees_flat.append(_tree_structure_to_flat(ts))
        else:
            trees_flat.append([])

    export = {
        "trees": trees_flat,
        "feature_names": FEATURE_NAMES,
        "num_features": len(FEATURE_NAMES),
        "ndcg_at_10": ndcg,
        "trained_at": datetime.now(timezone.utc).isoformat(),
    }

    os.makedirs(os.path.dirname(MODEL_PATH) or ".", exist_ok=True)
    with open(MODEL_PATH, "w") as f:
        json.dump(export, f, indent=2)

    log.info("Model exported to %s", MODEL_PATH)


if __name__ == "__main__":
    main()