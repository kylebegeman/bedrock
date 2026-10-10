#!/usr/bin/env python3
"""Ensure cleanup cannot adopt a container by name or malformed saved data."""
import importlib.util
import json
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

SPEC = importlib.util.spec_from_file_location("lane_vm", Path(__file__).parent / "vm/manage.py")
VM = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(VM)
IDENTITY = "1234567890abcdef1234567890abcdef"


class OwnedVM(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="bedrock-lane-vm-")
        self.addCleanup(self.temp.cleanup)
        self.work = Path(self.temp.name)
        self.state_patch = patch.object(VM, "STATE", self.work)
        self.state_patch.start()
        self.addCleanup(self.state_patch.stop)
        self.data = {"id": IDENTITY, "container": "bedrock-lane-" + IDENTITY}
        self.save()

    def save(self):
        (self.work / "machine.json").write_text(json.dumps(self.data))

    def test_foreign_container_name_is_refused_before_docker(self):
        self.data["container"] = "production-database"
        self.save()
        with patch.object(VM, "command") as command:
            with self.assertRaisesRegex(RuntimeError, "not a managed lane name"):
                VM.destroy()
            command.assert_not_called()

    def test_corrupted_identity_is_refused_before_docker(self):
        self.data["id"] = "not-an-identity"
        self.save()
        with patch.object(VM, "command") as command:
            with self.assertRaisesRegex(RuntimeError, "Invalid saved VM identity"):
                VM.destroy()
            command.assert_not_called()

    def test_similarly_named_container_without_ownership_label_is_refused(self):
        with patch.object(VM, "command", return_value='[{"Config":{"Labels":{}}}]') as command:
            with self.assertRaisesRegex(RuntimeError, "identity does not match"):
                VM.destroy()
            self.assertEqual(command.call_count, 1)
            self.assertEqual(command.call_args.args[:2], ("docker", "inspect"))

    def test_exact_owned_never_booted_vm_can_be_cleaned_up(self):
        info = [{"Config": {"Labels": {VM.LABEL: IDENTITY}}}]
        with patch.object(VM, "command", return_value=json.dumps(info)) as command:
            VM.destroy()
            self.assertEqual(command.call_args.args, ("docker", "rm", "-f", self.data["container"]))
            self.assertFalse((self.work / "machine.json").exists())

    def test_booted_vm_refusal_prevents_deletion(self):
        self.data["ready"] = True
        self.save()
        info = [{"Config": {"Labels": {VM.LABEL: IDENTITY}}}]
        with patch.object(VM, "command", return_value=json.dumps(info)) as command, patch.object(VM.subprocess, "run", side_effect=VM.subprocess.CalledProcessError(2, "guard")):
            with self.assertRaises(VM.subprocess.CalledProcessError):
                VM.destroy()
            self.assertEqual(command.call_count, 1)
            self.assertTrue((self.work / "machine.json").exists())


if __name__ == "__main__":
    unittest.main()
