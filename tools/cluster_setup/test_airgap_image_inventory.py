import argparse, hashlib, json, pathlib, tempfile, unittest
from unittest.mock import patch
import build_airgap_image_inventory as inv

class InventoryTests(unittest.TestCase):
    def setUp(self):
        self.tmp=tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root=pathlib.Path(self.tmp.name)

    def test_mirror_mapping_preserves_paths_and_strips_registry_host(self):
        self.assertEqual(inv.mirror_reference("mirror.example", "ghcr.io/team/agent:v1"),
                         "mirror.example/team/agent:v1")
        self.assertEqual(inv.mirror_reference("mirror.example", "team/agent:v1"),
                         "mirror.example/team/agent:v1")

    def test_image_list_deduplicates_and_skips_comments(self):
        p=self.root/"images.list"; p.write_text("# note\nx/a:v1\nx/a:v1\n")
        self.assertEqual(inv.image_references(p),["x/a:v1"])

    @patch.object(inv.subprocess,"run")
    def test_inspect_records_digest_and_multi_platforms(self,run):
        raw=json.dumps({"manifests":[
            {"digest":"sha256:amd","platform":{"os":"linux","architecture":"amd64"}},
            {"digest":"sha256:arm","platform":{"os":"linux","architecture":"arm64","variant":"v8"}}]})
        run.return_value.stdout=raw
        result=inv.inspect_image("x/a:v1")
        self.assertEqual(result["digest"],"sha256:"+hashlib.sha256(raw.encode()).hexdigest())
        self.assertEqual([p["architecture"] for p in result["platforms"]],["amd64","arm64"])

    @patch.object(inv.subprocess,"run")
    def test_registry_error_is_sanitized(self,run):
        import subprocess
        run.side_effect=subprocess.CalledProcessError(1,["skopeo"],stderr="unauthorized https://user:secret@registry")
        with self.assertRaisesRegex(RuntimeError,"authentication was rejected") as caught:
            inv.inspect_image("x/a:v1")
        self.assertNotIn("secret",str(caught.exception))

    def test_build_records_mirror_import_and_chart_hash(self):
        image_dir=self.root/"images"; image_dir.mkdir()
        (image_dir/"workload-images.list").write_text("x/las:v1\n")
        (image_dir/"workload-images.tar").write_bytes(b"archive")
        refs=self.root/"all.list"; refs.write_text("x/las:v1\n")
        chart=self.root/"las.tgz"; chart.write_bytes(b"chart")
        out=self.root/"inventory.json"
        args=argparse.Namespace(images_file=refs,image_lists_dir=image_dir,profile="cpuOnly",
            mirror_registry="mirror.example",embedded_chart=chart,output=out)
        with patch.object(inv,"inspect_image",return_value={"reference":"x/las:v1","digest":"sha256:"+"a"*64,
                "platforms":[{"os":"linux","architecture":"amd64"}]}):
            inv.build(args)
        result=json.loads(out.read_text()); image=result["images"][0]
        self.assertEqual(image["mirrorReference"],"mirror.example/x/las:v1")
        self.assertEqual(image["importArchives"],["images/workload-images.tar"])
        self.assertEqual(result["embeddedDistribution"]["sha256"],hashlib.sha256(b"chart").hexdigest())

    def test_validate_accepts_complete_bundle(self):
        bundle=self.root/"bundle"
        (bundle/"images").mkdir(parents=True)
        (bundle/"charts").mkdir()
        (bundle/"images/workload-images.list").write_text("x/las:v1\n")
        (bundle/"images/workload-images.tar").write_bytes(b"archive")
        (bundle/"charts/las.tgz").write_bytes(b"chart")
        data={
            "schemaVersion":1,"profile":"cpuOnly",
            "imageImports":[{"archive":"images/workload-images.tar","imageList":"images/workload-images.list"}],
            "images":[{"reference":"x/las:v1","digest":"sha256:"+"a"*64,
                       "platforms":[{"os":"linux","architecture":"amd64"}],
                       "importArchives":["images/workload-images.tar"]}],
            "embeddedDistribution":{"path":"charts/las.tgz","sha256":hashlib.sha256(b"chart").hexdigest()},
        }
        path=bundle/"image-inventory.json"; path.write_text(json.dumps(data))
        inv.validate(argparse.Namespace(inventory=path,bundle_dir=bundle))

    def test_validate_rejects_missing_archive(self):
        bundle=self.root/"bundle"; (bundle/"images").mkdir(parents=True)
        path=bundle/"image-inventory.json"
        path.write_text(json.dumps({
            "schemaVersion":1,"profile":"cpuOnly",
            "imageImports":[{"archive":"images/missing.tar","imageList":"images/missing.list"}],
            "images":[{"reference":"x/las:v1","digest":"sha256:"+"a"*64,
                       "platforms":[{"os":"linux","architecture":"amd64"}],
                       "importArchives":["images/missing.tar"]}],
        }))
        with self.assertRaisesRegex(ValueError,"archive is missing"):
            inv.validate(argparse.Namespace(inventory=path,bundle_dir=bundle))

    def test_validate_rejects_path_traversal(self):
        with self.assertRaisesRegex(ValueError,"unsafe bundle path"):
            inv.safe_bundle_path(self.root,"../escape")

if __name__=="__main__": unittest.main()
