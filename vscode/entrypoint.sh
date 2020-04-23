#!/bin/bash
set -e

if [[ $GIT_USER_NAME ]]; then
    git config --global user.name "$GIT_USER_NAME"
fi
if [[ $GIT_USER_EMAIL ]]; then
    git config --global user.email "$GIT_USER_EMAIL"
fi
if [ ! -e ".gitignore" ]; then
    if [[ $GIT_REPO_URLS ]]; then
        IFS=';'
        read -ra gits <<<"$GIT_REPO_URLS"
        for i in "${gits[@]}"; do  
            IFS='|'
            read -a strarr <<<"$i"
            gitUrl=${strarr[0]}
            gitBranch=${strarr[1]}
            gitDir=${strarr[2]}
            if [ -z "${gitBranch}" ]; then
                echo "git clone ${gitUrl} ${gitDir}"
                git clone ${gitUrl} ${gitDir}
            else
                echo "git clone -b ${gitBranch} ${gitUrl} ${gitDir}"
                git clone -b ${gitBranch} ${gitUrl} ${gitDir}
            fi
        done
    fi
    if [ ! -e ".gitignore" ]; then
        echo "" >> .gitignore
    fi
fi

exec $@
