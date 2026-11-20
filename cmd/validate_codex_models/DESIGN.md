# Catalog validation

`main` parses `--file`, reads the file with `os.ReadFile`, and passes the bytes to
`registry.ValidateCodexClientModelsJSON`.
The registry shares this validator with its catalog loader and remote refresh path, so local acceptance uses the same
rules as publication.

The rules require a non-empty model list, unique non-empty slugs, the `gpt-5.5` default template, required strings and
integer fields, valid context limits, and reasoning levels that include the default effort.
Validation returns the first error and leaves the input file unchanged.
The command performs local file validation without starting an updater.

`TestValidateCodexModelsCLI` builds the executable and invokes its public command line with synthetic files.
It checks success output, invalid JSON, empty catalogs, duplicate slugs, missing required fields, a missing default
template, unreadable paths, directory input, missing or blank `--file`, and unknown flags.
Each case checks the process exit code and stdout/stderr, and each file case checks that its content remains unchanged.
