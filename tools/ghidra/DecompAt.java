// Decompiles the functions at the given entry addresses.
import ghidra.app.script.GhidraScript;
import ghidra.app.decompiler.*;
import ghidra.program.model.listing.*;
public class DecompAt extends GhidraScript {
  @Override public void run() throws Exception {
    DecompInterface d = new DecompInterface(); d.openProgram(currentProgram);
    for (String a : getScriptArgs()) {
      Function f = getFunctionAt(toAddr(a));
      if (f == null) { println("==== no function at " + a); continue; }
      DecompileResults r = d.decompileFunction(f, 120, monitor);
      println("==== " + f.getName(true) + " @ " + f.getEntryPoint());
      println(r.decompileCompleted() ? r.getDecompiledFunction().getC() : "(failed)");
    }
  }
}
