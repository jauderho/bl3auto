# ClusterFuzzLite builder image.
# Copies the checked-out source (the code under test, including PRs) into the
# container rather than git-cloning main, then runs build.sh to compile fuzzers.
FROM gcr.io/oss-fuzz-base/base-builder-go@sha256:56e501922c95c648f144a451add2ddde979f8068fed695443a8dd0b8f5084bd2

COPY . $SRC/bl3auto
WORKDIR $SRC/bl3auto
COPY .clusterfuzzlite/build.sh $SRC/build.sh
