import unittest

from cross_check import fingerprint


class FingerprintTest(unittest.TestCase):
    def test_matches_usage_v2_fixture(self) -> None:
        detail = {
            "request_id": "req-123",
            "api_key": "api-key",
            "model": "gpt-5.6-sol",
            "timestamp": "2026-08-30T12:34:56.123456789Z",
            "source": "source.json",
            "auth_index": "7",
            "provider": "openai",
            "failed": True,
            "input_tokens": 1,
            "output_tokens": 2,
            "reasoning_tokens": 3,
            "cached_tokens": 4,
            "total_tokens": 10,
        }

        self.assertEqual(
            fingerprint(detail),
            "231fde333a097488867b61419171d9c4ddb24f1a",
        )


if __name__ == "__main__":
    unittest.main()
