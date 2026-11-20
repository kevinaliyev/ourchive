package vfs

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"runtime"
	"strings"
)

var ErrPickerCancelled = errors.New("folder selection cancelled")

// PickFolderNative opens the operating system's modern native File Explorer / Finder
// folder picker dialog on the machine running the Agent and returns the selected path.
func PickFolderNative(ctx context.Context) (string, error) {
	return pickNative(ctx, true)
}

// PickFileNative opens the operating system's modern native File Explorer / Finder
// single-file picker dialog so a user can reference an individual file without copying it.
func PickFileNative(ctx context.Context) (string, error) {
	return pickNative(ctx, false)
}

func pickNative(ctx context.Context, foldersOnly bool) (string, error) {
	switch runtime.GOOS {
	case "windows":
		// Uses the modern Windows IFileOpenDialog COM interface with Per-Monitor DPI Awareness v2.
		optFlags := "0x40 | 0x1000" // FOS_FORCEFILESYSTEM | FOS_FILEMUSTEXIST
		title := "Choose a file to reference in Ourchive"
		if foldersOnly {
			optFlags = "0x20 | 0x40 | 0x800" // FOS_PICKFOLDERS | FOS_FORCEFILESYSTEM | FOS_PATHMUSTEXIST
			title = "Choose a folder to share with Ourchive"
		}
		script := `
$code = @"
using System;
using System.Runtime.InteropServices;

public static class ModernDialogPicker {
    [ComImport, Guid("DC1C5A9C-E88A-4dde-A5A1-60F82A20AEF7")]
    private class FileOpenDialog {}

    [ComImport, Guid("42f85136-db7e-439c-85f1-e4075d135fc8"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    private interface IFileDialog {
        [PreserveSig] int Show(IntPtr parent);
        void SetFileTypes(uint cFileTypes, IntPtr rgFilterSpec);
        void SetFileTypeIndex(uint iFileType);
        void GetFileTypeIndex(out uint piFileType);
        void Advise(IntPtr pfde, out uint pdwCookie);
        void Unadvise(uint dwCookie);
        void SetOptions(uint fos);
        void GetOptions(out uint pfos);
        void SetDefaultFolder(IShellItem psi);
        void SetFolder(IShellItem psi);
        void GetFolder(out IShellItem ppsi);
        void GetCurrentSelection(out IShellItem ppsi);
        void SetFileName([MarshalAs(UnmanagedType.LPWStr)] string pszName);
        void GetFileName([MarshalAs(UnmanagedType.LPWStr)] out string pszName);
        void SetTitle([MarshalAs(UnmanagedType.LPWStr)] string pszTitle);
        void SetOkButtonLabel([MarshalAs(UnmanagedType.LPWStr)] string pszText);
        void SetFileNameLabel([MarshalAs(UnmanagedType.LPWStr)] string pszLabel);
        void GetResult(out IShellItem ppsi);
        void AddPlace(IShellItem psi, int alignment);
        void SetDefaultExtension([MarshalAs(UnmanagedType.LPWStr)] string pszDefaultExtension);
        void Close(int hr);
        void SetClientGuid(ref Guid guid);
        void ClearClientData();
        void SetFilter(IntPtr pFilter);
    }

    [ComImport, Guid("43826D1E-E718-42EE-BC55-A1E261C37BFE"), InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    private interface IShellItem {
        void BindToHandler(IntPtr pbc, ref Guid bhid, ref Guid riid, out IntPtr ppv);
        void GetParent(out IShellItem ppsi);
        void GetDisplayName(uint sigdnName, [MarshalAs(UnmanagedType.LPWStr)] out string ppszName);
        void GetAttributes(uint sfgaoMask, out uint psfgaoAttribs);
        void Compare(IShellItem psi, uint hint, out int piOrder);
    }

    [DllImport("user32.dll")]
    private static extern bool SetProcessDpiAwarenessContext(IntPtr value);

    public static string Pick(string title) {
        try { SetProcessDpiAwarenessContext(new IntPtr(-4)); } catch {}
        var dialog = (IFileDialog)new FileOpenDialog();
        try {
            uint options;
            dialog.GetOptions(out options);
            dialog.SetOptions(options | ` + optFlags + `);
            dialog.SetTitle(title);
            int hr = dialog.Show(IntPtr.Zero);
            if (hr != 0) return "";
            IShellItem item;
            dialog.GetResult(out item);
            string path;
            item.GetDisplayName(0x80058000, out path);
            return path ?? "";
        } finally {
            Marshal.ReleaseComObject(dialog);
        }
    }
}
"@
Add-Type -TypeDefinition $code
$picked = [ModernDialogPicker]::Pick("` + title + `")
if ($picked) { [Console]::Out.Write($picked) }
`
		cmd := exec.CommandContext(ctx, "powershell", "-NoProfile", "-STA", "-Command", script)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return "", err
		}
		picked := strings.TrimSpace(out.String())
		if picked == "" {
			return "", ErrPickerCancelled
		}
		return picked, nil

	case "darwin":
		osa := `POSIX path of (choose file with prompt "Choose a file to reference in Ourchive")`
		if foldersOnly {
			osa = `POSIX path of (choose folder with prompt "Choose a folder to share with Ourchive")`
		}
		cmd := exec.CommandContext(ctx, "osascript", "-e", osa)
		var out bytes.Buffer
		cmd.Stdout = &out
		if err := cmd.Run(); err != nil {
			return "", ErrPickerCancelled
		}
		picked := strings.TrimSpace(out.String())
		if picked == "" {
			return "", ErrPickerCancelled
		}
		return strings.TrimRight(picked, "/"), nil

	default:
		if zenity, err := exec.LookPath("zenity"); err == nil {
			args := []string{"--file-selection", "--title=Choose a file to reference in Ourchive"}
			if foldersOnly {
				args = []string{"--file-selection", "--directory", "--title=Choose a folder to share with Ourchive"}
			}
			cmd := exec.CommandContext(ctx, zenity, args...)
			var out bytes.Buffer
			cmd.Stdout = &out
			if err := cmd.Run(); err != nil {
				return "", ErrPickerCancelled
			}
			picked := strings.TrimSpace(out.String())
			if picked == "" {
				return "", ErrPickerCancelled
			}
			return picked, nil
		}
		return "", errors.New("no graphical picker available on this headless machine; enter a path manually")
	}
}
