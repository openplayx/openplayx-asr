# Contributing

Thanks for your interest. Bug reports, fixes and small improvements are welcome.

## Issues

- Search existing issues first.
- For bugs, include the version (`openplayx-asr -version`), the platform, the
  model, the request you sent and the response or log you got. Remove tokens
  and private addresses before posting, and do not attach audio you cannot
  share.
- Security problems: do **not** open a public issue, see [SECURITY.md](SECURITY.md).

## Pull requests

1. Open an issue first for anything larger than a small fix, so we can agree
   on the approach.
2. Keep changes focused; one topic per pull request.
3. Run the tests before pushing:

   ```sh
   (cd src && go test ./...)   # fake engine, no native build needed
   scripts/build.sh            # native build against package-whisper
   ```

4. The HTTP API (`src/openapi.yaml`), MCP tools and error codes are a contract
   with the OpenPlayX app and other clients. Keep `openapi.yaml` in sync with
   the code and call out breaking changes in the pull request.
5. The engine is reached through the `opx_asr` C ABI from
   [package-whisper](https://github.com/openplayx/package-whisper). ABI changes
   start there and must keep the Dart and Go sides returning the same result
   structure.
6. Update the README in both languages (`README.md` and `README.zh-CN.md`)
   when behaviour, configuration or the API change.

By contributing you agree that your contribution is licensed under this
repository's license.
