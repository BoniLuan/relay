"""Offline checks for channel ownership and private credential creation."""
from pathlib import Path
import runpy
import tempfile
import unittest
from unittest.mock import patch

SETUP = runpy.run_path(str(Path(__file__).resolve().parents[1] / "setup-telegram.py"))


class TelegramSetupTests(unittest.TestCase):
    def select(self, member, answer="y"):
        function = SETUP["channel_destination"]
        replies = [{"id": 123}, {"type": "channel", "id": -100123}, member]
        with patch.dict(function.__globals__, api=lambda *args: replies.pop(0)):
            with patch("builtins.input", return_value=answer), patch("builtins.print"):
                return function("fake", -100123)

    def test_channel_requires_posting_permission_and_confirmation(self):
        admin = {"status": "administrator", "can_post_messages": True}
        self.assertEqual(self.select(admin), -100123)
        for member in [{"status": "member"}, {"status": "administrator", "can_post_messages": False}]:
            with self.subTest(member=member), self.assertRaises(ValueError):
                self.select(member)
        with self.assertRaises(ValueError):
            self.select(admin, "n")

    def test_credentials_are_private_and_never_overwritten(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "credential"
            SETUP["private_write"](path, "fake")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)
            with self.assertRaises(FileExistsError):
                SETUP["private_write"](path, "replacement")
            self.assertEqual(path.read_text(), "fake\n")

    def test_redirects_are_refused(self):
        with self.assertRaises(ValueError):
            SETUP["NoRedirect"]().redirect_request(None, None, 302, "", {}, "https://example.org")


if __name__ == "__main__":
    unittest.main()
