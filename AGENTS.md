# Agent memory system usin DuckDB with duckpgq

## Tech Stack
- Go Lang
- DuckDB + duckpgq
- Vector search is an exact cosine scan; do not add a persisted vss/HNSW index (its checkpoints leak blocks, see README)

## Automated Test Methodology
- No E2E Tests
- No tests that assert the existance of some code, variable, constant, etc.
- Always the smallest possible set of tests (Low noise high signal)
