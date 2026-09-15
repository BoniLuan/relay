import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("prepare", Path(__file__).parents[1] / "prepare-monitoring.py")
prepare = importlib.util.module_from_spec(spec)
spec.loader.exec_module(prepare)


class IntegrationTest(unittest.TestCase):
    def test_preserves_existing_settings_without_mutation(self):
        base = {"global": {"scrape_interval": "17s"}, "scrape_configs": [{"job_name": "vigil"}], "rule_files": ["existing.yml"], "alerting": {"alertmanagers": []}}
        added = {"scrape_configs": [{"job_name": "relay-metrics"}]}
        result = prepare.integrate(base, added)
        self.assertEqual(result["scrape_configs"], base["scrape_configs"] + added["scrape_configs"])
        self.assertEqual(result["global"], base["global"])
        self.assertEqual(result["alerting"], base["alerting"])
        self.assertEqual(base["rule_files"], ["existing.yml"])
        self.assertEqual(result["rule_files"], ["existing.yml", "/etc/prometheus/relay/alerts.yml"])
        with self.assertRaises(ValueError):
            prepare.integrate(result, added)

    def test_telegram_preserves_existing_notification_routes(self):
        base = {"alerting": {"alertmanagers": [{"static_configs": [{"targets": ["existing:9093"]}]}], "alert_relabel_configs": [{"action": "labeldrop", "regex": "private"}]}}
        result = prepare.integrate(base, {"scrape_configs": [{"job_name": "relay-metrics"}]}, telegram=True)
        self.assertEqual(len(result["alerting"]["alertmanagers"]), 2)
        self.assertEqual(result["alerting"]["alertmanagers"][0], base["alerting"]["alertmanagers"][0])
        self.assertEqual(result["alerting"]["alert_relabel_configs"], base["alerting"]["alert_relabel_configs"])
        self.assertEqual(len(base["alerting"]["alertmanagers"]), 1)


if __name__ == "__main__":
    unittest.main()
