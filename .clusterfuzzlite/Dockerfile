# ClusterFuzzLite builder image.
# Copies the checked-out source (the code under test, including PRs) into the
# container rather than git-cloning main, then runs build.sh to compile fuzzers.
FROM gcr.io/oss-fuzz-base/base-builder-go@sha256:e0412f1c703fd2e7508d6ed558ccba58aec3bb996f39e9d325ceda3bb1dd3dfe

COPY . $SRC/bl3auto
WORKDIR $SRC/bl3auto
COPY .clusterfuzzlite/build.sh $SRC/build.sh
