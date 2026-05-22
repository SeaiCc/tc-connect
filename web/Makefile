
ALL_AGENTS    := acp claudecode codex cursor gemini iflow opencode pi qoder

COMMA := ,
_EXCLUDE_TAGS :=

ifdef AGENTS
  _WANTED_AGENTS := $(subst $(COMMA), ,$(AGENTS)) # 逗号替换为空格 opencode, acp -> opencode acp
  _EXCLUDE_AGENTS := $(filter-out $(_WANTED_AGENTS),$(ALL_AGENTS)) # 取差集 caludecode cursor
  _EXCLUDE_TAGS += $(addprefix no_,$(_EXCLUDE_AGENTS)) # no_caludecode, no_cursor
endif

_BUILD_TAGS := $(strip $(_EXCLUDE_TAGS))
_TAGS_FLAG  := $(if $(_BUILD_TAGS),-tags '$(_BUILD_TAGS)',)

debug:
	@echo "ALL_AGENTS: $(ALL_AGENTS)"
	@echo "AGENTS: $(AGENTS)"
	@echo "_WANTED_AGENTS: $(_WANTED_AGENTS)"
	@echo "_EXCLUDE_AGENTS: $(_EXCLUDE_AGENTS)"
	@echo "_EXCLUDE_TAGS: $(_EXCLUDE_TAGS)"
	@echo "_BUILD_TAGS: $(_BUILD_TAGS)"
	@echo "_TAGS_FLAG: $(_TAGS_FLAG)"
