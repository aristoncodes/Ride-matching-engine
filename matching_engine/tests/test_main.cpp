// The one translation unit that compiles Catch2's main(). Kept alone in its
// own file on purpose: the header is 640KB and expanding it twice would double
// the suite's build time for nothing.
#define CATCH_CONFIG_MAIN
#include "catch.hpp"
