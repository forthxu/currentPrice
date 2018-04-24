#!/bin/bash
PATH=/usr/local/bin:/bin:/usr/bin:/usr/local/sbin:/usr/sbin:/sbin

if [ "$(uname)" == "Darwin" ];then
	make mac
elif [ "$(expr substr $(uname -s) 1 5)" == "Linux" ];then
	make linux
elif [ "$(expr substr $(uname -s) 1 10)" == "MINGW32_NT" ];then
	make windows
fi