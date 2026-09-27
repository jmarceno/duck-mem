# Agent memory system usin DuckDB with duckpgq and vss

## Tech Stack
Go Lang
DuckDB + duckpgq + vss

## Iteration 1:
Naive system where we dump everything from a session, minus tool call into the database.
To search, agent can query duckdb
Each project is a column ? (Must validate that, I'm not too versed in DuckDB)

## Iteration 2:
CLI for the query


## Initial database load for Development and local tests
Load all Codex, Cursor and Claude sessions from this machine into the database as a test sample

## Automated Test Methodology
- No E2E Tests
- No tests that assert the existance of some code, variable, constant, etc.
- Always the smallest possible set of tests (Low noise high signal)
