pwd = $(PWD)
after ?= $(shell date +%F --date='-1days')
before ?= $(shell date +%F --date='+1days')
proj_dirs = $(dir $(wildcard ../*/))
authors = ldc,ldc0,ldcc,ludc,Ldc fzkun
author ?= $(authors)

run: $(authors:=.user)

%.user:
	@rm -f $(pwd)/$*.txt
	make -s fetch author='$*'
	LOG_DATA=$* npm start

fetch: $(proj_dirs:=.dir)
	@echo Sussessed Build: '$(after) -> $(before)' $(author).txt

comma:=,
%.dir:
	git --git-dir=$*.git \
	log --date=short \
	--author=$(subst $(comma),\\\|,$(author)) \
	--after=$(after) \
	--no-merges --all >> $(pwd)/$(author).txt
	@echo '\n' >> $(pwd)/$(author).txt



