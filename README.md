# fps-demo

CoD2 demo archive for the homelab. The service imports the `matches.json` export produced by [fps-challenge-parser](/Users/apy19/workspace/personal/fps-challenge-parser), keeps searchable metadata in SQLite, and downloads each demo into a persistent filesystem archive.

## Local development

```sh
go run . import -file /path/to/matches.json
go run . serve
```

The UI is at `http://localhost:8080`. Set `DB_PATH` and `DEMO_ROOT` to change storage locations. `sync` runs `PARSER_COMMAND`, then imports `PARSER_MATCHES_FILE`; this is intended for the daily Kubernetes CronJob.

## Storage estimate

The PVC is initially 50 GiB. At roughly 5–20 MiB per downloaded CoD2 demo, 3–4 months of 30 demos/day is about 4.5–18 GiB, plus SQLite/index overhead and a generous safety margin. Confirm actual sizes after the first week and resize the PVC if the real daily rate is higher.

For production, build and publish the image, then adapt `k8s.yaml` to the homelab’s image registry, parser data PVC, and Gateway hostname. The CronJob currently assumes the parser executable and its `matches.json` are available on the mounted parser volume.
