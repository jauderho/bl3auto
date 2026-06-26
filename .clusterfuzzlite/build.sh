#!/bin/bash -eu
# ClusterFuzzLite build script.
#
# Compiles the native Go fuzz targets (testing.F functions in *_test.go) into
# libFuzzer binaries for ClusterFuzzLite/OSS-Fuzz.
#
# Usage: invoked automatically by the build_fuzzers action inside the
# base-builder-go container. Not meant to be run directly.
#
# compile_native_go_fuzzer <package> <FuzzFunc> <output-binary-name>

cd "$SRC/bl3auto"

# Native Go fuzz targets (testing.F) are converted by compile_native_go_fuzzer
# into libFuzzer harnesses that import this shim in place of stdlib testing.
# It is only needed for the instrumented OSS-Fuzz build, so it is pulled here
# rather than committed to go.mod (local `go test -fuzz` uses real testing).
go get github.com/AdamKorcz/go-118-fuzz-build/testing

compile_native_go_fuzzer github.com/jauderho/bl3auto FuzzParseCodeList   FuzzParseCodeList
compile_native_go_fuzzer github.com/jauderho/bl3auto FuzzStatusFromText  FuzzStatusFromText
compile_native_go_fuzzer github.com/jauderho/bl3auto FuzzParseRetryAfter FuzzParseRetryAfter
