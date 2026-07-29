# ShinGo Edge Makefile
#
# Minimal on purpose — Edge is built and deployed by install-edge.sh, not from
# here. This exists so `make schema-snapshot` means the same thing in both
# modules, which is what the staleness test's failure message tells you to run.

.PHONY: build test vet schema-snapshot

build:
	go build ./...

test:
	go test ./...

vet:
	go vet ./...

# Regenerate the committed schema snapshot: create an empty SQLite database,
# run the FULL migrate path, read sqlite_master, write
# store/schema/schema.snapshot.sql.
#
# No Docker and no external tools — SQLite is a file. TestSchemaSnapshotIsCurrent
# fails with this command's name when the file goes stale, and it runs in the
# default suite rather than behind the docker tag.
schema-snapshot:
	go run ./cmd/schemasnapshot
