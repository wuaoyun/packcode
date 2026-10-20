# RAR 5 test fixture

`rar5-stored.rar.uu` is the unmodified `test_read_format_rar5_stored.rar.uu` fixture from [libarchive](https://github.com/libarchive/libarchive/blob/master/libarchive/test/test_read_format_rar5_stored.rar.uu). Its expected member is `helloworld.txt`, containing `hello libarchive test suite!` followed by a newline.

`rar5-compressed.rar.uu` and `rar5-solid.rar.uu` are the unmodified `test_read_format_rar5_compressed.rar.uu` and `test_read_format_rar5_multiple_files_solid.rar.uu` fixtures from the same upstream directory. Tests compare their contents against Windows libarchive's independent decoder when available.

The upstream license is retained in `LIBARCHIVE-LICENSE.txt`. RAR 4 stored fixtures used by the tests are generated independently in `formats_test.go`.
