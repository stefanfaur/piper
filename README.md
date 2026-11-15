# piper

A small ETL service for ATAD Project 6. You describe a pipeline in JSON. The service runs it and tells you which rows it rejected and why.

It uses only the Go standard library and keeps all state in memory.

## Run

```sh
docker build -t piper .
docker run -p 8080:8080 piper
```

Without Docker: `go run .`

## Example

```sh
curl -H "Content-Type: application/json" -d '{
  "name": "clean-sales",
  "source": {"type": "inline", "rows": [
    {"prod": "desk",  "qty": "2", "unit_price": "150.00"},
    {"prod": "lamp",  "qty": "1", "unit_price": "19.99"},
    {"prod": "chair", "qty": "x", "unit_price": "75.00"}
  ]},
  "steps": [
    {"op": "rename",  "from": "prod", "to": "product"},
    {"op": "compute", "field": "total", "expr": "qty * unit_price"},
    {"op": "filter",  "where": "total >= 100"}
  ],
  "destination": "sales_clean"
}' localhost:8080/pipelines

curl -X POST localhost:8080/pipelines/1/run   # read 3, written 1, rejected 1
curl localhost:8080/runs/1                    # chair: qty: "x" is not a number
curl localhost:8080/data/sales_clean          # the desk row
```

## Pipeline format

**Sources**

| Type | Fields |
|---|---|
| `inline` | `rows`: a list of objects |
| `csv` | `data`: CSV text, the first line is the header |
| `table` | `name`: a destination that an earlier run filled |

**Steps**

| Op | Fields |
|---|---|
| `rename` | `from`, `to` |
| `select` | `fields` |
| `compute` | `field`, `expr` |
| `filter` | `where` |
| `validate` | `rule`, `on_fail` (`reject` or `abort`) |
| `aggregate` | `group_by`, `fn` (`sum`, `count`, `avg`, `min`, `max`), `field`, `as` |

Expressions use Go syntax: field names, numbers, strings, `+ - * /`, comparisons, `&&`, `||`, `!`. A string that looks like a number counts as a number.

**Destination**

Use a name for an in-memory table. Use `{"type": "file", "name": "x"}` to write `x.json` in `DATA_DIR`.

**Schedule**

Add `"schedule": "30s"` to run the pipeline at that interval.

## Routes

| Route | Method | Result |
|---|---|---|
| `/pipelines` | POST, GET | Define a pipeline, list pipelines |
| `/pipelines/{id}/run` | POST | Run now |
| `/runs` | GET | All runs |
| `/runs/{id}` | GET | One report with every rejected row |
| `/data/{destination}` | GET | Rows in a destination |
| `/health`, `/version`, `/` | GET | Service information |
| `/reset` | POST | Delete everything |

## When a row fails

- A row that a step cannot process is rejected. The report stores its row number, the step, the reason and the row. The run continues.
- A row that `filter` removes is not a rejection.
- If more than half of the rows are rejected, the run fails.
- If a `validate` step has `on_fail: abort`, the first bad row fails the run.
- A failed run writes nothing. The service builds the output in memory and replaces the destination in one step, after the last row.

## Add a step

Write a builder function in `steps.go` and add it to the `ops` map.

## Test

```sh
go test -race ./...
```
