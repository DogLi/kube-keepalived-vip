#!/usr/bin/env python
# -*- coding: utf-8 -*-


import os
import sys
import time
import os.path
import argparse
import subprocess


def get_parser():
    parser = argparse.ArgumentParser(prog="build", description='Tool for build golang operator')
    group = parser.add_mutually_exclusive_group()
    group.add_argument("--branch", "-b",
                           action="store",
                           required=False,
                           dest="branch",
                           help="checkout to the branch of the code")
    group.add_argument("--tag", "-t",
                           action="store",
                           required=False,
                           dest="tag",
                           help="checkout to the tag")
    group.add_argument("--cid", "-c",
                           action="store",
                           required=False,
                           dest="commitid",
                           help="checkout to the commit id")
    parser.add_argument("--version", "-v",
                           action="store",
                           required=True,
                           dest="version",
                           help="set package version")
    parser.add_argument("--output", "-o",
                           action="store",
                           required=True,
                           dest="output",
                           help="set the output path")
    parser.add_argument("--source", "-s",
                           action="store",
                           required=True,
                           dest="source",
                           help="set the source path")

    return parser

def run(cmd):
    p = subprocess.Popen(cmd, shell=True, close_fds=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    out_msg = p.stdout.read()
    err_msg = p.stderr.read()
    exit_code = p.wait()
    if err_msg:
        print err_msg
        raise Exception("run cmd `{}` error: {}".format(cmd, err_msg))
    else:
        return out_msg.strip()

# Get last tag.
def get_last_tag():
    return run('git describe --abbrev=0 --tags')

# Get current branch name.
def get_current_branch():
    return run('git rev-parse --abbrev-ref HEAD')

# Get last git commit id.
def get_last_commitid():
    return run('git log --pretty=format:"%h" -1')

# Assemble build command.
def build(source, output, version, branchName=None, commitID=None):
    checkout = branchName or commitID
    if checkout:
        cmd = "git checkout {}".format(checkout)
        print(cmd)
        run(cmd)

    buildFlag = ["-s", "-w"]

    buildFlag.append("-X 'main._version_={}'".format(version))

    branchName = branchName or get_current_branch()
    if branchName != "":
        buildFlag.append("-X 'main._branch_={}'".format(branchName))

    commitId = commitID or get_last_commitid()
    if commitId != "":
        buildFlag.append("-X 'main._commitId_={}'".format(commitId))

    # current time
    buildFlag.append("-X 'main._buildTime_={}'".format(time.strftime("%Y-%m-%d %H:%M %z")))

    cmd = 'CGO_ENABLED=0 GOOS=linux go build  -a -installsuffix cgo -o {output} -ldflags "{flag}" {go_file}'.format(output=output,flag=" ".join(buildFlag), go_file=source)
    run(cmd)

def main(argv):
    go_root = os.environ.get('GOROOT')
    if not go_root:
        print("Please set the GOROOT env")
        return
    else:
        print("current GOROOT: {}".format(go_root))
    go_path = os.environ.get('GOPATH')
    if go_path:
        print "current GOPATH: {}".format(go_path)
    else:
        current_path = os.path.dirname(os.path.abspath(__file__))
        print "set current path as GOPATH: {}".format(current_path)
        os.environ["GOPATH"] = current_path
    parser = get_parser()
    args = parser.parse_args(argv)
    branchName = args.branch
    commitID = args.commitid
    source = args.source
    output = args.output
    version = args.version
    build(source, output, version, branchName, commitID)
    print("build finished! The binary file is:  {}".format(output))

if __name__ == "__main__":
    argv = sys.argv
    main(argv[1:])
