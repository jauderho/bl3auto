# ClusterFuzzLite builder image.
# Copies the checked-out source (the code under test, including PRs) into the
# container rather than git-cloning main, then runs build.sh to compile fuzzers.
FROM gcr.io/oss-fuzz-base/base-builder-go@sha256:b09aefba29091d3ce938e44cc60940b94f3dec72f7d81816fdd84615ab421f66

COPY . $SRC/bl3auto
WORKDIR $SRC/bl3auto
COPY .clusterfuzzlite/build.sh $SRC/build.sh
