# Synthetic episode corpus

Synthetic OpenCode 2.0 sessions with expected episode labels, for the eval episode
discovery tests. `gen.py` writes every case; the expected labels in it were written by
hand before the discovery rules existed and are frozen: a rule change must not edit them.

- `dev/`: cases for development.
- `heldout/`: cases that development does not look at; quality is measured on them.
- `heldout-seg/`: held-out cases for the optional local-model segmentation (two tasks
  without a check between them, an unclassified request, a correction that must stay,
  interleaved tasks). Frozen on 2026-10-18 before the segmentation code and before any
  model run; `model_segmented` marks a boundary or family that the model proposed.

Each case directory holds `case.json` (snapshot file trees, session exports with
`SNAP:<name>` snapshot placeholders, the pass time) and `expected.json` (episodes:
requests, corrections, base and result snapshots, edited paths, attribution, outcome,
family, links, reasons). All content is synthetic.
