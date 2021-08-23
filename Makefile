repdt ?= $(shell date +%F)
after = $(shell date +%F --date='$(repdt) -1days')
before = $(shell date +%F --date='$(after) +1days')
authors = ldc\\\|ldc0\\\|ldcc\\\|ludc\\\|Ldc
proj_dirs = $(dir $(wildcard ../*/))
pwd = $(PWD)

run: $(proj_dirs:=.dir)
	@echo '$(after) -> $(before)'
	@echo Sussessed Build: report.txt
	npm start


%.dir: clean
	@cd $* && \
	git log --date=short \
	--author=$(authors) \
	--after=$(after) \
	--before=$(before) \
	--no-merges --all >> $(pwd)/report.txt

clean:
	@rm -f $(pwd)/report.txt



