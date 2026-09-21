# ClusterFuzzLite builder image.
# Copies the checked-out source (the code under test, including PRs) into the
# container rather than git-cloning main, then runs build.sh to compile fuzzers.
FROM gcr.io/oss-fuzz-base/base-builder-go@sha256:c74d701a74af681e18bde2db89775711c074001b10f38460675a4bc0dc60d760

COPY . $SRC/bl3auto
WORKDIR $SRC/bl3auto
COPY .clusterfuzzlite/build.sh $SRC/build.sh
