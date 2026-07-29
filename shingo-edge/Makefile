# ShinGo Edge Makefile
#
# Minimal on purpose — Edge is built and deployed by install-edge.sh, not from
# here. This exists so `make schema-snapshot` means the same thing in both
# modules, which is what the staleness test's failure message tells you to run.

.PHONY: build test vet schema-snapshot defaults-snapshot

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

# Regenerate the committed rendering of the SHIPPED defaults.
#
# Phase 1 applied to behaviour instead of schema: the defaults are how this
# system behaves when nobody has said otherwise, there was no file you could
# open to see them, and retuning one is a one-character edit that is invisible
# in review. With the rendering committed, it is a diff somebody can question.
#
# It pins the shipped default, NOT a plant's yaml — a plant overriding a value
# locally does not touch this file. Changing what everyone gets is what should
# require acknowledgement.
defaults-snapshot:
	go run ./cmd/defaultssnapshot
