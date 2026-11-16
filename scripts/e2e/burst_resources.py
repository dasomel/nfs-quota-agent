"""Shared Stage D2/D3 bulk manifest construction (no cluster operations)."""

import copy


def burst_objects(templates, names, export, label_key, label):
  objects = []
  for name in names:
    for original in templates:
      obj = copy.deepcopy(original)
      obj["metadata"]["name"] = name
      obj["metadata"].setdefault("labels", {})[label_key] = label
      if obj["kind"] == "PersistentVolume":
        obj["spec"]["nfs"]["path"] = f"{export}/{name}"
      else:
        obj["spec"]["volumeName"] = name
      objects.append(obj)
  return objects
