// Copyright 2018, Pulumi Corporation.  All rights reserved.

package engine

import (
	"bytes"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/sergi/go-diff/diffmatchpatch"

	"github.com/pulumi/pulumi/pkg/diag/colors"
	"github.com/pulumi/pulumi/pkg/resource"
	"github.com/pulumi/pulumi/pkg/resource/deploy"
	"github.com/pulumi/pulumi/pkg/util/contract"
)

// GetIndent computes a step's parent indentation.
func GetIndent(step StepEventMetadata, seen map[resource.URN]StepEventMetadata) int {
	indent := 0
	for p := step.Res.Parent; p != ""; {
		if par, has := seen[p]; !has {
			// This can happen during deletes, since we delete children before parents.
			// TODO[pulumi/pulumi#340]: we need to figure out how best to display this sequence; at the very
			//     least, it would be ideal to preserve the indentation.
			break
		} else {
			indent++
			p = par.Res.Parent
		}
	}
	return indent
}

func printStepHeader(b *bytes.Buffer, step StepEventMetadata) {
	var extra string
	old := step.Old
	new := step.New
	if new != nil && !new.Protect && old != nil && old.Protect {
		// show an unlocked symbol, since we are unprotecting a resource.
		extra = " 🔓"
	} else if (new != nil && new.Protect) || (old != nil && old.Protect) {
		// show a locked symbol, since we are either newly protecting this resource, or retaining protection.
		extra = " 🔒"
	}
	writeString(b, fmt.Sprintf("%s: (%s)%s\n", string(step.Type), step.Op, extra))
}

func getIndentationString(indent int, op deploy.StepOp, prefix bool) string {
	var result string
	for i := 0; i < indent; i++ {
		result += "    "
	}

	if result == "" {
		contract.Assertf(!prefix, "Expected indention for a prefixed line")
		return result
	}

	var rp string
	if prefix {
		rp = op.RawPrefix()
	} else {
		rp = "  "
	}
	contract.Assert(len(rp) == 2)
	contract.Assert(len(result) >= 2)
	return result[:len(result)-2] + rp
}

func writeString(b *bytes.Buffer, s string) {
	_, err := b.WriteString(s)
	contract.IgnoreError(err)
}

func writeWithIndent(b *bytes.Buffer, indent int, op deploy.StepOp, prefix bool, format string, a ...interface{}) {
	writeString(b, op.Color())
	writeString(b, getIndentationString(indent, op, prefix))
	writeString(b, fmt.Sprintf(format, a...))
	writeString(b, colors.Reset)
}

func writeWithIndentNoPrefix(b *bytes.Buffer, indent int, op deploy.StepOp, format string, a ...interface{}) {
	writeWithIndent(b, indent, op, false, format, a...)
}

func write(b *bytes.Buffer, op deploy.StepOp, format string, a ...interface{}) {
	writeWithIndentNoPrefix(b, 0, op, format, a...)
}

func writeVerbatim(b *bytes.Buffer, op deploy.StepOp, value string) {
	writeWithIndentNoPrefix(b, 0, op, "%s", value)
}

func GetResourcePropertiesSummary(step StepEventMetadata, indent int) string {
	var b bytes.Buffer

	op := step.Op
	urn := step.URN
	old := step.Old

	// Print the indentation.
	writeString(&b, getIndentationString(indent, op, false))

	// First, print out the operation's prefix.
	writeString(&b, op.Prefix())

	// Next, print the resource type (since it is easy on the eyes and can be quickly identified).
	printStepHeader(&b, step)

	// For these simple properties, print them as 'same' if they're just an update or replace.
	simplePropOp := considerSameIfNotCreateOrDelete(op)

	// Print out the URN and, if present, the ID, as "pseudo-properties" and indent them.
	var id resource.ID
	if old != nil {
		id = old.ID
	}

	// Always print the ID and URN.
	if id != "" {
		writeWithIndentNoPrefix(&b, indent+1, simplePropOp, "[id=%s]\n", string(id))
	}
	if urn != "" {
		writeWithIndentNoPrefix(&b, indent+1, simplePropOp, "[urn=%s]\n", urn)
	}

	return b.String()
}

func GetResourcePropertiesDetails(
	step StepEventMetadata, indent int, planning bool, summary bool, debug bool) string {
	var b bytes.Buffer

	// indent everything an additional level, like other properties.
	indent++

	var replaces []resource.PropertyKey
	if step.Op == deploy.OpCreateReplacement || step.Op == deploy.OpReplace {
		replaces = step.Keys
	}

	old := step.Old
	new := step.New

	if old == nil && new != nil {
		printObject(&b, new.Inputs, planning, indent, step.Op, false, debug)
	} else if new == nil && old != nil {
		// in summary view, we don't have to print out the entire object that is getting deleted.
		// note, the caller will have already printed out the type/name/id/urn of the resource,
		// and that's sufficient for a summarized deletion view.
		if !summary {
			printObject(&b, old.Inputs, planning, indent, step.Op, false, debug)
		}
	} else {
		printOldNewDiffs(&b, old.Inputs, new.Inputs, replaces, planning, indent, step.Op, summary, debug)
	}

	return b.String()
}

func maxKey(keys []resource.PropertyKey) int {
	maxkey := 0
	for _, k := range keys {
		if len(k) > maxkey {
			maxkey = len(k)
		}
	}
	return maxkey
}

func printObject(
	b *bytes.Buffer, props resource.PropertyMap, planning bool,
	indent int, op deploy.StepOp, prefix bool, debug bool) {

	// Compute the maximum with of property keys so we can justify everything.
	keys := props.StableKeys()
	maxkey := maxKey(keys)

	// Now print out the values intelligently based on the type.
	for _, k := range keys {
		if v := props[k]; shouldPrintPropertyValue(v, planning) {
			printPropertyTitle(b, string(k), maxkey, indent, op, prefix)
			printPropertyValue(b, v, planning, indent, op, prefix, debug)
		}
	}
}

// GetResourceOutputsPropertiesString prints only those properties that either differ from the input properties or, if
// there is an old snapshot of the resource, differ from the prior old snapshot's output properties.
func GetResourceOutputsPropertiesString(
	step StepEventMetadata, indent int, planning bool, debug bool) string {

	var b bytes.Buffer

	// Only certain kinds of steps have output properties associated with them.
	new := step.New
	if new == nil || new.Outputs == nil {
		return ""
	}
	op := considerSameIfNotCreateOrDelete(step.Op)

	// First fetch all the relevant property maps that we may consult.
	ins := new.Inputs
	outs := new.Outputs

	// Now sort the keys and enumerate each output property in a deterministic order.
	firstout := true
	keys := outs.StableKeys()
	maxkey := maxKey(keys)
	for _, k := range keys {
		out := outs[k]
		// Print this property if it is printable and either ins doesn't have it or it's different.
		if shouldPrintPropertyValue(out, true) {
			var print bool
			if in, has := ins[k]; has {
				print = (out.Diff(in) != nil)
			} else {
				print = true
			}

			if print {
				if firstout {
					writeWithIndentNoPrefix(&b, indent, op, "---outputs:---\n")
					firstout = false
				}
				printPropertyTitle(&b, string(k), maxkey, indent, op, false)
				printPropertyValue(&b, out, planning, indent, op, false, debug)
			}
		}
	}

	return b.String()
}

func considerSameIfNotCreateOrDelete(op deploy.StepOp) deploy.StepOp {
	if op == deploy.OpCreate || op == deploy.OpDelete || op == deploy.OpDeleteReplaced {
		return op
	}

	return deploy.OpSame
}

func shouldPrintPropertyValue(v resource.PropertyValue, outs bool) bool {
	if v.IsNull() {
		return false // don't print nulls (they just clutter up the output).
	}
	if v.IsString() && v.StringValue() == "" {
		return false // don't print empty strings either.
	}
	if v.IsArray() && len(v.ArrayValue()) == 0 {
		return false // skip empty arrays, since they are often uninteresting default values.
	}
	if v.IsObject() && len(v.ObjectValue()) == 0 {
		return false // skip objects with no properties, since they are also uninteresting.
	}
	if v.IsObject() && len(v.ObjectValue()) == 0 {
		return false // skip objects with no properties, since they are also uninteresting.
	}
	if v.IsOutput() && !outs {
		// also don't show output properties until the outs parameter tells us to.
		return false
	}
	return true
}

func printPropertyTitle(b *bytes.Buffer, name string, align int, indent int, op deploy.StepOp, prefix bool) {
	writeWithIndent(b, indent, op, prefix, "%-"+strconv.Itoa(align)+"s: ", name)
}

func printPropertyValue(
	b *bytes.Buffer, v resource.PropertyValue, planning bool,
	indent int, op deploy.StepOp, prefix bool, debug bool) {

	if v.IsNull() {
		writeVerbatim(b, op, "<null>")
	} else if v.IsBool() {
		write(b, op, "%t", v.BoolValue())
	} else if v.IsNumber() {
		write(b, op, "%v", v.NumberValue())
	} else if v.IsString() {
		write(b, op, "%q", v.StringValue())
	} else if v.IsArray() {
		arr := v.ArrayValue()
		if len(arr) == 0 {
			writeVerbatim(b, op, "[]")
		} else {
			writeVerbatim(b, op, "[\n")
			for i, elem := range arr {
				writeWithIndent(b, indent, op, prefix, "    [%d]: ", i)
				printPropertyValue(b, elem, planning, indent+1, op, prefix, debug)
			}
			writeWithIndentNoPrefix(b, indent, op, "]")
		}
	} else if v.IsAsset() {
		a := v.AssetValue()
		if a.IsText() {
			write(b, op, "asset(text:%s) {\n", shortHash(a.Hash))

			a = resource.MassageIfUserProgramCodeAsset(a, debug)

			massaged := a.Text

			// pretty print the text, line by line, with proper breaks.
			lines := strings.Split(massaged, "\n")
			for _, line := range lines {
				writeWithIndentNoPrefix(b, indent, op, "    %s\n", line)
			}
			writeWithIndentNoPrefix(b, indent, op, "}")
		} else if path, has := a.GetPath(); has {
			write(b, op, "asset(file:%s) { %s }", shortHash(a.Hash), path)
		} else {
			contract.Assert(a.IsURI())
			write(b, op, "asset(uri:%s) { %s }", shortHash(a.Hash), a.URI)
		}
	} else if v.IsArchive() {
		a := v.ArchiveValue()
		if assets, has := a.GetAssets(); has {
			write(b, op, "archive(assets:%s) {\n", shortHash(a.Hash))
			var names []string
			for name := range assets {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				printAssetOrArchive(b, assets[name], name, planning, indent, op, prefix, debug)
			}
			writeWithIndentNoPrefix(b, indent, op, "}")
		} else if path, has := a.GetPath(); has {
			write(b, op, "archive(file:%s) { %s }", shortHash(a.Hash), path)
		} else {
			contract.Assert(a.IsURI())
			write(b, op, "archive(uri:%s) { %v }", shortHash(a.Hash), a.URI)
		}
	} else if v.IsComputed() || v.IsOutput() {
		// We render computed and output values differently depending on whether or not we are planning or deploying:
		// in the former case, we display `computed<type>` or `output<type>`; in the former we display `undefined`.
		// This is because we currently cannot distinguish between user-supplied undefined values and input properties
		// that are undefined because they were sourced from undefined values in other resources' output properties.
		// Once we have richer information about the dataflow between resources, we should be able to do a better job
		// here (pulumi/pulumi#234).
		if planning {
			writeVerbatim(b, op, v.TypeString())
		} else {
			write(b, op, "undefined")
		}
	} else {
		contract.Assert(v.IsObject())
		obj := v.ObjectValue()
		if len(obj) == 0 {
			writeVerbatim(b, op, "{}")
		} else {
			writeVerbatim(b, op, "{\n")
			printObject(b, obj, planning, indent+1, op, prefix, debug)
			writeWithIndentNoPrefix(b, indent, op, "}")
		}
	}
	writeVerbatim(b, op, "\n")
}

func printAssetOrArchive(
	b *bytes.Buffer, v interface{}, name string, planning bool,
	indent int, op deploy.StepOp, prefix bool, debug bool) {
	writeWithIndent(b, indent, op, prefix, "    \"%v\": ", name)
	printPropertyValue(b, assetOrArchiveToPropertyValue(v), planning, indent+1, op, prefix, debug)
}

func assetOrArchiveToPropertyValue(v interface{}) resource.PropertyValue {
	switch t := v.(type) {
	case *resource.Asset:
		return resource.NewAssetProperty(t)
	case *resource.Archive:
		return resource.NewArchiveProperty(t)
	default:
		contract.Failf("Unexpected archive element '%v'", reflect.TypeOf(t))
		return resource.PropertyValue{V: nil}
	}
}

func shortHash(hash string) string {
	if len(hash) > 7 {
		return hash[:7]
	}
	return hash
}

func printOldNewDiffs(
	b *bytes.Buffer, olds resource.PropertyMap, news resource.PropertyMap,
	replaces []resource.PropertyKey, planning bool, indent int, op deploy.StepOp,
	summary bool, debug bool) {

	// Get the full diff structure between the two, and print it (recursively).
	if diff := olds.Diff(news); diff != nil {
		printObjectDiff(b, *diff, replaces, false, planning, indent, summary, debug)
	} else {
		printObject(b, news, planning, indent, op, true, debug)
	}
}

func printObjectDiff(b *bytes.Buffer, diff resource.ObjectDiff,
	replaces []resource.PropertyKey, causedReplace bool, planning bool,
	indent int, summary bool, debug bool) {

	contract.Assert(indent > 0)

	// Compute the maximum with of property keys so we can justify everything.
	keys := diff.Keys()
	maxkey := maxKey(keys)

	// If a list of what causes a resource to get replaced exist, create a handy map.
	var replaceMap map[resource.PropertyKey]bool
	if len(replaces) > 0 {
		replaceMap = make(map[resource.PropertyKey]bool)
		for _, k := range replaces {
			replaceMap[k] = true
		}
	}

	// To print an object diff, enumerate the keys in stable order, and print each property independently.
	for _, k := range keys {
		titleFunc := func(top deploy.StepOp, prefix bool) {
			printPropertyTitle(b, string(k), maxkey, indent, top, prefix)
		}
		if add, isadd := diff.Adds[k]; isadd {
			if shouldPrintPropertyValue(add, planning) {
				printAdd(b, add, titleFunc, planning, indent, debug)
			}
		} else if delete, isdelete := diff.Deletes[k]; isdelete {
			if shouldPrintPropertyValue(delete, planning) {
				printDelete(b, delete, titleFunc, planning, indent, debug)
			}
		} else if update, isupdate := diff.Updates[k]; isupdate {
			if !causedReplace && replaceMap != nil {
				causedReplace = replaceMap[k]
			}

			printPropertyValueDiff(
				b, titleFunc, update, causedReplace, planning,
				indent, summary, debug)
		} else if same := diff.Sames[k]; !summary && shouldPrintPropertyValue(same, planning) {
			titleFunc(deploy.OpSame, false)
			printPropertyValue(b, diff.Sames[k], planning, indent, deploy.OpSame, false, debug)
		}
	}
}

func printPropertyValueDiff(
	b *bytes.Buffer, titleFunc func(deploy.StepOp, bool),
	diff resource.ValueDiff, causedReplace bool, planning bool,
	indent int, summary bool, debug bool) {

	op := deploy.OpUpdate
	contract.Assert(indent > 0)

	if diff.Array != nil {
		titleFunc(op, true)
		writeVerbatim(b, op, "[\n")

		a := diff.Array
		for i := 0; i < a.Len(); i++ {
			elemTitleFunc := func(eop deploy.StepOp, eprefix bool) {
				writeWithIndent(b, indent+1, eop, eprefix, "[%d]: ", i)
			}
			if add, isadd := a.Adds[i]; isadd {
				printAdd(b, add, elemTitleFunc, planning, indent+2, debug)
			} else if delete, isdelete := a.Deletes[i]; isdelete {
				printDelete(b, delete, elemTitleFunc, planning, indent+2, debug)
			} else if update, isupdate := a.Updates[i]; isupdate {
				printPropertyValueDiff(
					b, elemTitleFunc, update, causedReplace, planning,
					indent+2, summary, debug)
			} else if !summary {
				elemTitleFunc(deploy.OpSame, false)
				printPropertyValue(b, a.Sames[i], planning, indent+2, deploy.OpSame, false, debug)
			}
		}
		writeWithIndentNoPrefix(b, indent, op, "]\n")
	} else if diff.Object != nil {
		titleFunc(op, true)
		writeVerbatim(b, op, "{\n")
		printObjectDiff(b, *diff.Object, nil, causedReplace, planning, indent+1, summary, debug)
		writeWithIndentNoPrefix(b, indent, op, "}\n")
	} else {
		shouldPrintOld := shouldPrintPropertyValue(diff.Old, false)
		shouldPrintNew := shouldPrintPropertyValue(diff.New, false)

		if diff.Old.IsArchive() &&
			diff.New.IsArchive() &&
			!causedReplace &&
			shouldPrintOld &&
			shouldPrintNew {
			printArchiveDiff(
				b, titleFunc, diff.Old.ArchiveValue(), diff.New.ArchiveValue(),
				planning, indent, summary, debug)
			return
		}

		// If we ended up here, the two values either differ by type, or they have different primitive values.  We will
		// simply emit a deletion line followed by an addition line.
		if shouldPrintOld {
			printDelete(b, diff.Old, titleFunc, planning, indent, debug)
		}
		if shouldPrintNew {
			printAdd(b, diff.New, titleFunc, planning, indent, debug)
		}
	}
}

func printDelete(
	b *bytes.Buffer, v resource.PropertyValue, title func(deploy.StepOp, bool),
	planning bool, indent int, debug bool) {
	op := deploy.OpDelete
	title(op, true)
	printPropertyValue(b, v, planning, indent, op, true, debug)
}

func printAdd(
	b *bytes.Buffer, v resource.PropertyValue, title func(deploy.StepOp, bool),
	planning bool, indent int, debug bool) {
	op := deploy.OpCreate
	title(op, true)
	printPropertyValue(b, v, planning, indent, op, true, debug)
}

func printArchiveDiff(
	b *bytes.Buffer, titleFunc func(deploy.StepOp, bool),
	oldArchive *resource.Archive, newArchive *resource.Archive,
	planning bool, indent int, summary bool, debug bool) {

	// TODO: this could be called recursively from itself.  In the recursive case, we might have an
	// archive that actually hasn't changed.  Check for that, and terminate the diff printing.

	op := deploy.OpUpdate

	hashChange := getTextChangeString(shortHash(oldArchive.Hash), shortHash(newArchive.Hash))

	if oldPath, has := oldArchive.GetPath(); has {
		if newPath, has := newArchive.GetPath(); has {
			titleFunc(op, true)
			write(b, op, "archive(file:%s) { %s }\n", hashChange, getTextChangeString(oldPath, newPath))
			return
		}
	} else if oldURI, has := oldArchive.GetURI(); has {
		if newURI, has := newArchive.GetURI(); has {
			titleFunc(op, true)
			write(b, op, "archive(uri:%s) { %s }\n", hashChange, getTextChangeString(oldURI, newURI))
			return
		}
	} else {
		contract.Assert(oldArchive.IsAssets())
		oldAssets, _ := oldArchive.GetAssets()

		if newAssets, has := newArchive.GetAssets(); has {
			titleFunc(op, true)
			write(b, op, "archive(assets:%s) {\n", hashChange)
			printAssetsDiff(b, oldAssets, newAssets, planning, indent+1, summary, debug)
			writeWithIndentNoPrefix(b, indent, deploy.OpUpdate, "}\n")
			return
		}
	}

	// Type of archive changed, print this out as an remove and an add.
	printDelete(
		b, assetOrArchiveToPropertyValue(oldArchive),
		titleFunc, planning, indent, debug)
	printAdd(
		b, assetOrArchiveToPropertyValue(newArchive),
		titleFunc, planning, indent, debug)
}

func printAssetsDiff(
	b *bytes.Buffer,
	oldAssets map[string]interface{}, newAssets map[string]interface{},
	planning bool, indent int, summary bool, debug bool) {

	// Diffing assets proceeds by getting the sorted list of asset names from both the old and
	// new assets, and then stepwise processing each.  For any asset in old that isn't in new,
	// we print this out as a delete.  For any asset in new that isn't in old, we print this out
	// as an add.  For any asset in both we print out of it is unchanged or not.  If so, we
	// recurse on that data to print out how it changed.

	var oldNames []string
	var newNames []string

	for name := range oldAssets {
		oldNames = append(oldNames, name)
	}

	for name := range newAssets {
		newNames = append(newNames, name)
	}

	sort.Strings(oldNames)
	sort.Strings(newNames)

	i := 0
	j := 0

	var keys []resource.PropertyKey
	for _, name := range oldNames {
		keys = append(keys, "\""+resource.PropertyKey(name)+"\"")
	}
	for _, name := range newNames {
		keys = append(keys, "\""+resource.PropertyKey(name)+"\"")
	}

	maxkey := maxKey(keys)

	for i < len(oldNames) || j < len(newNames) {
		deleteOld := false
		addNew := false
		if i < len(oldNames) && j < len(newNames) {
			oldName := oldNames[i]
			newName := newNames[j]

			if oldName == newName {
				titleFunc := func(top deploy.StepOp, tprefix bool) {
					printPropertyTitle(b, "\""+oldName+"\"", maxkey, indent, top, tprefix)
				}

				oldAsset := oldAssets[oldName]
				newAsset := newAssets[newName]

				switch t := oldAsset.(type) {
				case *resource.Archive:
					printArchiveDiff(
						b, titleFunc, t, newAsset.(*resource.Archive),
						planning, indent, summary, debug)
				case *resource.Asset:
					printAssetDiff(
						b, titleFunc, t, newAsset.(*resource.Asset),
						planning, indent, summary, debug)
				}

				i++
				j++
				continue
			}

			if oldName < newName {
				deleteOld = true
			} else {
				addNew = true
			}
		} else if i < len(oldNames) {
			deleteOld = true
		} else {
			addNew = true
		}

		newIndent := indent + 1
		if deleteOld {
			oldName := oldNames[i]
			titleFunc := func(top deploy.StepOp, tprefix bool) {
				printPropertyTitle(b, "\""+oldName+"\"", maxkey, indent, top, tprefix)
			}
			printDelete(
				b, assetOrArchiveToPropertyValue(oldAssets[oldName]),
				titleFunc, planning, newIndent, debug)
			i++
			continue
		} else {
			contract.Assert(addNew)
			newName := newNames[j]
			titleFunc := func(top deploy.StepOp, tprefix bool) {
				printPropertyTitle(b, "\""+newName+"\"", maxkey, indent, top, tprefix)
			}
			printAdd(
				b, assetOrArchiveToPropertyValue(newAssets[newName]),
				titleFunc, planning, newIndent, debug)
			j++
		}
	}
}

func makeAssetHeader(asset *resource.Asset) string {
	var assetType string
	var contents string

	if path, has := asset.GetPath(); has {
		assetType = "file"
		contents = path
	} else if uri, has := asset.GetURI(); has {
		assetType = "uri"
		contents = uri
	} else {
		assetType = "text"
		contents = "..."
	}

	return fmt.Sprintf("asset(%s:%s) { %s }\n", assetType, shortHash(asset.Hash), contents)
}

func printAssetDiff(
	b *bytes.Buffer, titleFunc func(deploy.StepOp, bool),
	oldAsset *resource.Asset, newAsset *resource.Asset,
	planning bool, indent int, summary bool, debug bool) {

	op := deploy.OpUpdate

	// If the assets aren't changed, just print out: = assetName: type(hash)
	if oldAsset.Hash == newAsset.Hash {
		if !summary {
			op = deploy.OpSame
			titleFunc(op, false)
			write(b, op, makeAssetHeader(oldAsset))
		}
		return
	}

	// if the asset changed, print out: ~ assetName: type(hash->hash) details...

	hashChange := getTextChangeString(shortHash(oldAsset.Hash), shortHash(newAsset.Hash))

	if oldAsset.IsText() {
		if newAsset.IsText() {
			titleFunc(deploy.OpUpdate, true)
			write(b, op, "asset(text:%s) {\n", hashChange)

			massagedOldText := resource.MassageIfUserProgramCodeAsset(oldAsset, debug).Text
			massagedNewText := resource.MassageIfUserProgramCodeAsset(newAsset, debug).Text

			differ := diffmatchpatch.New()
			differ.DiffTimeout = 0

			hashed1, hashed2, lineArray := differ.DiffLinesToChars(massagedOldText, massagedNewText)
			diffs1 := differ.DiffMain(hashed1, hashed2, false)
			diffs2 := differ.DiffCharsToLines(diffs1, lineArray)

			writeString(b, diffToPrettyString(diffs2, indent+1))

			writeWithIndentNoPrefix(b, indent, op, "}\n")
			return
		}
	} else if oldPath, has := oldAsset.GetPath(); has {
		if newPath, has := newAsset.GetPath(); has {
			titleFunc(deploy.OpUpdate, true)
			write(b, op, "asset(file:%s) { %s }\n", hashChange, getTextChangeString(oldPath, newPath))
			return
		}
	} else {
		contract.Assert(oldAsset.IsURI())

		oldURI, _ := oldAsset.GetURI()
		if newURI, has := newAsset.GetURI(); has {
			titleFunc(deploy.OpUpdate, true)
			write(b, op, "asset(uri:%s) { %s }\n", hashChange, getTextChangeString(oldURI, newURI))
			return
		}
	}

	// Type of asset changed, print this out as an remove and an add.
	printDelete(
		b, assetOrArchiveToPropertyValue(oldAsset),
		titleFunc, planning, indent, debug)
	printAdd(
		b, assetOrArchiveToPropertyValue(newAsset),
		titleFunc, planning, indent, debug)
}

func getTextChangeString(old string, new string) string {
	if old == new {
		return old
	}

	return fmt.Sprintf("%s->%s", old, new)
}

// diffToPrettyString takes the full diff produed by diffmatchpatch and condenses it into something
// useful we can print to the console.  Specifically, while it includes any adds/removes in
// green/red, it will also show portions of the unchanged text to help give surrounding context to
// those add/removes. Because the unchanged portions may be very large, it only included around 3
// lines before/after the change.
func diffToPrettyString(diffs []diffmatchpatch.Diff, indent int) string {
	var buff bytes.Buffer

	writeDiff := func(op deploy.StepOp, text string) {
		var prefix bool
		if op == deploy.OpCreate || op == deploy.OpDelete {
			prefix = true
		}
		writeWithIndent(&buff, indent, op, prefix, "%s", text)
	}

	for index, diff := range diffs {
		text := diff.Text
		lines := strings.Split(text, "\n")
		printLines := func(op deploy.StepOp, startInclusive int, endExclusive int) {
			for i := startInclusive; i < endExclusive; i++ {
				if strings.TrimSpace(lines[i]) != "" {
					writeDiff(op, lines[i])
					buff.WriteString("\n")
				}
			}
		}

		switch diff.Type {
		case diffmatchpatch.DiffInsert:
			printLines(deploy.OpCreate, 0, len(lines))
		case diffmatchpatch.DiffDelete:
			printLines(deploy.OpDelete, 0, len(lines))
		case diffmatchpatch.DiffEqual:
			var trimmedLines []string
			for _, line := range lines {
				if strings.TrimSpace(line) != "" {
					trimmedLines = append(trimmedLines, line)
				}
			}
			lines = trimmedLines

			const contextLines = 2

			// Show the unchanged text in white.
			if index == 0 {
				// First chunk of the file.
				if len(lines) > contextLines+1 {
					writeDiff(deploy.OpSame, "...\n")
					printLines(deploy.OpSame, len(lines)-contextLines, len(lines))
					continue
				}
			} else if index == len(diffs)-1 {
				if len(lines) > contextLines+1 {
					printLines(deploy.OpSame, 0, contextLines)
					writeDiff(deploy.OpSame, "...\n")
					continue
				}
			} else {
				if len(lines) > (2*contextLines + 1) {
					printLines(deploy.OpSame, 0, contextLines)
					writeDiff(deploy.OpSame, "...\n")
					printLines(deploy.OpSame, len(lines)-contextLines, len(lines))
					continue
				}
			}

			printLines(deploy.OpSame, 0, len(lines))
		}
	}

	return buff.String()
}