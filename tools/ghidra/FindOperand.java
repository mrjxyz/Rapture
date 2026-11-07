// Prints every instruction in the program whose text contains the given substring.
import ghidra.app.script.GhidraScript;
import ghidra.program.model.listing.*;
public class FindOperand extends GhidraScript {
  @Override public void run() throws Exception {
    String needle = getScriptArgs()[0];
    for (Instruction ins : currentProgram.getListing().getInstructions(true)) {
      String t = ins.toString();
      if (t.contains(needle)) {
        Function f = getFunctionContaining(ins.getAddress());
        println(ins.getAddress() + " " + (f == null ? "?" : f.getName(true)) + " " + t);
      }
    }
  }
}
