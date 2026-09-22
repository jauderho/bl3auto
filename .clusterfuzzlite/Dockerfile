# ClusterFuzzLite builder image.
# Copies the checked-out source (the code under test, including PRs) into the
# container rather than git-cloning main, then runs build.sh to compile fuzzers.
FROM gcr.io/oss-fuzz-base/base-builder-go@sha256:68d51737a25f21ead9fe46292db205694d9000c369aa1f380c73d4e8647924a5

COPY . $SRC/bl3auto
WORKDIR $SRC/bl3auto
COPY .clusterfuzzlite/build.sh $SRC/build.sh
