# Filtered retrieval and embedding caches

`rag.QdrantTextIndex.QueryWithFilter` embeds a text query and applies metadata
conditions in Qdrant before selecting `topK`. `QdrantIndex.QueryVector` accepts a
precomputed vector. Existing `QdrantTextIndex.Query` calls remain valid and use no
caller filter. The low-level `QdrantIndex.Query` cannot embed text and returns an
error directing the caller to `QueryVector`.

## Typed metadata conditions

A `MetadataFilter` is an AND list of conditions constructed with:

| Constructor | Qdrant operation |
|---|---|
| `MetadataEqualString(key, value)` | Exact keyword match |
| `MetadataEqualBool(key, value)` | Boolean match, including `false` |
| `MetadataEqualInt(key, value)` | Exact `int64` match |
| `MetadataEqualFloat(key, value)` | Inclusive range with both endpoints equal |
| `MetadataInRange(key, NumericRange{...})` | Numeric lower/upper bounds |

`NumericRange` accepts `GT` or `GTE` for its lower bound and `LT` or `LTE` for its
upper bound. Values are `*float64`; nil means no bound. The constructor copies
pointed-to values, so later caller mutation cannot change a condition. At least
one bound is required. Nonfinite values, duplicate lower/upper bounds, reversed
ranges and empty exclusive ranges are rejected before embedding or RPC. Ranges
have float64 precision; integer equality retains the full int64 value. A zero
`MetadataCondition` is invalid, and unsupported value types have no constructor.

Keys address the Go index's **top-level payload**, not Python's
`chunk.metadata` nesting. Empty keys, control characters, nested-path syntax
(`.`, brackets, backslash, quotes), `content` and the configured vector key are
rejected. Use ordinary metadata fields such as `tenant`, `category` or `rating`.

Both Qdrant configs accept `RequiredFilter`, copied at construction. Every query
ANDs it with caller conditions. A contradictory caller condition returns no
matches; it cannot override the configured condition. This is a retrieval
constraint, not a tenant authorization system. The host remains responsible for
authentication, collection access, document ingestion and other access paths.

```go
minimum := 4.0
filter := rag.MetadataFilter{
    rag.MetadataEqualBool("published", true),
    rag.MetadataInRange("rating", rag.NumericRange{GTE: &minimum}),
}
docs, err := index.QueryWithFilter(ctx, "agent orchestration", 5, filter)
```

The [standalone example](../examples/qdrant_filter/main.go) uses fixed vectors
and a real local Qdrant server on gRPC port 6334. It creates a uniquely named
collection, stores three documents, verifies filtering before `topK=1`, and
removes its collection on exit:

```bash
# Start a compatible Qdrant server separately, then:
go run ./examples/qdrant_filter
```

The example checks filter semantics, not embedding quality. Unit tests use the
real Go client against a local gRPC fixture to verify serialization and errors;
a fixture is not a Qdrant integration test. `AddDocuments` retains its asynchronous
upsert behavior, so the example waits for query visibility. Existing query result
metadata retains Qdrant value objects. Ingestion stores the configured vector in
Qdrant's native vector field, omitting that internal key from payload. Empty or
nonfinite vectors and unsupported payload types return errors before any batch
upsert. Arbitrary metadata retains Qdrant client conversion semantics; this is
not a general metadata schema validator.

## Cache write semantics

`embedding.FileEmbeddingCache.Store` marshals vectors before deciding whether
an entry fits its configured byte limit. An oversized entry is skipped with nil
error, preserving existing entries, including an old value under the same key.
The cache is best effort; use content-addressed keys instead of treating `Store`
as a guaranteed overwrite. Zero or negative limits remain unlimited.

Accepted entries use a same-directory temporary file, file sync and rename via
`fsutil.WriteFileAtomic`. Write or replacement failure returns an error; a failed
replacement does not truncate the old file. The existing JSON format and
same-instance mutex remain. This is not a cross-process transaction, shared cache
namespace, tenant isolation guarantee or promise of directory-fsync durability.

`rag/parser.ChunkText` drops entirely blank windows in both character and
approximate-token modes. Nonblank chunk content and overlap positions remain
unchanged; parser document indices are contiguous after filtering.
