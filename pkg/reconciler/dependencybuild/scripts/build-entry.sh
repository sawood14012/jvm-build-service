#!/usr/bin/env bash
set -o verbose
set -eu
set -o pipefail

cp -r -a  /original-content/* $(workspaces.source.path)
cd $(workspaces.source.path)/workspace

if [ -n "$(params.CONTEXT_DIR)" ]
then
    cd $(params.CONTEXT_DIR)
fi

echo "JAVA_HOME:$JAVA_HOME"
if [ ! -z ${MAVEN_HOME+x} ]; then
    echo "MAVEN_HOME:$MAVEN_HOME"
    PATH="${MAVEN_HOME}/bin:$PATH"
fi

if [ ! -z ${GRADLE_HOME+x} ]; then
    echo "GRADLE_HOME:$GRADLE_HOME"
    PATH="${GRADLE_HOME}/bin:$PATH"
fi

if [ ! -z ${ANT_HOME+x} ]; then
    echo "ANT_HOME:$ANT_HOME"
    PATH="${ANT_HOME}/bin:$PATH"
fi

if [ ! -z ${SBT_DIST+x} ]; then
    echo "SBT_DIST:$SBT_DIST"
    PATH="${SBT_DIST}/bin:$PATH"
fi
echo "PATH:$PATH"

#fix this when we no longer need to run as root
export HOME=/root

mkdir -p $(workspaces.source.path)/logs $(workspaces.source.path)/packages $(workspaces.source.path)/build-info

{{INSTALL_PACKAGE_SCRIPT}}

#This is replaced when the task is created by the golang code
{{PRE_BUILD_SCRIPT}}

{{BUILD}}

{{POST_BUILD_SCRIPT}}
