"""
Evaluation script for the L2R model.
Loads model, computes NDCG@10, MAP, precision@5, and compares to baseline.
"""

import json
import logging
import os

import numpy as np

from .features import FEATURE_NAMES, extract_features

log = logging.getLogger(__name__)

MODEL_PATH = "/data/l2r_model.json"
DB_PATH = os.getenv("DB_PATH", "/data/clipfeed.db")


def _predict_one_sample(x: np.ndarray, trees: list, feature_names: list) -> float:
    """Score one sample using the exported tree ensemble."""
    score = 0.0
    for tree in trees:
        if not tree:
            continue
        node_idx = 0
        while True:
            node = tree[node_idx]
            if node.get("is_leaf", False):
                score += node["leaf_value"]
                break
            fidx = node["feature_index"]
            thresh = node["threshold"]
            if fidx < 0 or fidx >= len(x):
                break
            if x[fidx] <= thresh:
                node_idx = node["left_child"]
            else:
                node_idx = node["right_child"]
            if node_idx < 0 or node_idx >= len(tree):
                break
    return score


def _predict(X: np.ndarray, model: dict) -> np.ndarray:
    """Batch prediction using exported model."""
    trees = model.get("trees", [])
    feature_names = model.get("feature_names", [])
    preds = np.empty(len(X), dtype=np.float64)
    for i in range(len(X)):
        preds[i] = _predict_one_sample(X[i], trees, feature_names)
    return preds


def _ndcg_at_k(
    y_true: np.ndarray, y_score: np.ndarray, group_sizes: np.ndarray, k: int = 10
) -> float:
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


def _map_at_k(
    y_true: np.ndarray, y_score: np.ndarray, group_sizes: np.ndarray, k: int = 10
) -> float:
    """Compute mean Average Precision@k across groups. Binary relevance: >= 0.5."""
    from sklearn.metrics import average_precision_score

    if len(group_sizes) == 0 or group_sizes.sum() == 0:
        return 0.0
    aps = []
    offset = 0
    for g in group_sizes:
        if g == 0:
            continue
        yg = (y_true[offset : offset + g] >= 0.5).astype(np.float64)
        sg = y_score[offset : offset + g]
        offset += g
        if g < 2 or yg.sum() == 0:
            continue
        try:
            aps.append(average_precision_score(yg, sg))
        except Exception:
            continue
    return float(np.mean(aps)) if aps else 0.0


def _precision_at_k(
    y_true: np.ndarray, y_score: np.ndarray, group_sizes: np.ndarray, k: int = 5
) -> float:
    """Compute mean Precision@k across groups. Binary relevance: >= 0.5."""
    if len(group_sizes) == 0 or group_sizes.sum() == 0:
        return 0.0
    precs = []
    offset = 0
    for g in group_sizes:
        if g == 0:
            continue
        yg = y_true[offset : offset + g]
        sg = y_score[offset : offset + g]
        offset += g
        top_k = min(k, g)
        if top_k == 0:
            continue
        order = np.argsort(-sg)[:top_k]
        relevant = np.sum((yg[order] >= 0.5).astype(np.float64))
        precs.append(relevant / top_k)
    return float(np.mean(precs)) if precs else 0.0


def main() -> None:
    """Load model, evaluate on data, compare to baseline."""
    logging.basicConfig(level=logging.INFO, format="%(asctime)s [%(levelname)s] %(message)s")

    if not os.path.isfile(MODEL_PATH):
        log.error("Model not found at %s", MODEL_PATH)
        return

    with open(MODEL_PATH) as f:
        model = json.load(f)

    X, y, group_sizes = extract_features(DB_PATH)
    if len(X) == 0:
        log.warning("No evaluation data available")
        return

    # Use same 80/20 group split as training for fair evaluation
    from .train import _split_by_groups

    _, _, _, X_test, y_test, g_test = _split_by_groups(X, y, group_sizes, test_size=0.2, seed=42)
    if len(X_test) == 0:
        log.warning("No test samples after split")
        return

    # Model predictions
    y_pred = _predict(X_test, model)

    # Baseline: content_score only (first feature)
    content_idx = FEATURE_NAMES.index("content_score") if "content_score" in FEATURE_NAMES else 0
    y_baseline = X_test[:, content_idx]

    ndcg_l2r = _ndcg_at_k(y_test, y_pred, g_test, k=10)
    ndcg_base = _ndcg_at_k(y_test, y_baseline, g_test, k=10)
    map_l2r = _map_at_k(y_test, y_pred, g_test, k=10)
    map_base = _map_at_k(y_test, y_baseline, g_test, k=10)
    p5_l2r = _precision_at_k(y_test, y_pred, g_test, k=5)
    p5_base = _precision_at_k(y_test, y_baseline, g_test, k=5)

    print("=" * 60)
    print("L2R Model Evaluation")
    print("=" * 60)
    print(f"Test samples: {len(X_test)}, Test groups: {len(g_test)}")
    print()
    print(f"{'Metric':<16} {'L2R Model':>12} {'Baseline (content_score)':>24} {'Delta':>10}")
    print("-" * 60)
    print(f"{'NDCG@10':<16} {ndcg_l2r:>12.4f} {ndcg_base:>24.4f} {ndcg_l2r - ndcg_base:>+10.4f}")
    print(f"{'MAP@10':<16} {map_l2r:>12.4f} {map_base:>24.4f} {map_l2r - map_base:>+10.4f}")
    print(f"{'Precision@5':<16} {p5_l2r:>12.4f} {p5_base:>24.4f} {p5_l2r - p5_base:>+10.4f}")
    print("=" * 60)
    print(f"Model trained at: {model.get('trained_at', 'N/A')}")
    print(f"Stored NDCG@10:   {model.get('ndcg_at_10', 'N/A')}")


if __name__ == "__main__":
    main()
